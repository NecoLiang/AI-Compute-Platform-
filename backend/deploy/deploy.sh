#!/bin/sh
set -eu

release_tag=${1:-}
case "$release_tag" in
  ""|*[!0-9a-f]*)
    echo "release tag must contain only lowercase hexadecimal characters" >&2
    exit 2
    ;;
esac

deploy_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$deploy_dir/compose.yml"
env_file="$deploy_dir/.env"
image="wanxiang-backend:$release_tag"
candidate="wanxiang-backend-candidate"

for path in "$compose_file" "$env_file"; do
  if [ ! -s "$path" ]; then
    echo "required deployment file is missing or empty: $path" >&2
    exit 1
  fi
done

for key in DATABASE_DSN REDIS_PASSWORD JWT_ACCESS_SECRET JWT_REFRESH_SECRET SECURITY_CREDENTIAL_KEY; do
  if ! grep -q "^${key}=." "$env_file"; then
    echo "required deployment variable is missing: $key" >&2
    exit 1
  fi
done

for network in config_default wanxiang-frontend_default; do
  docker network inspect "$network" >/dev/null
done

for dependency in wanxiang-mysql wanxiang-redis; do
  state=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$dependency")
  if [ "$state" != "healthy" ] && [ "$state" != "running" ]; then
    echo "$dependency is not ready: $state" >&2
    exit 1
  fi
done

docker image inspect "$image" >/dev/null
docker rm -f "$candidate" >/dev/null 2>&1 || true

cleanup() {
  docker rm -f "$candidate" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker run -d \
  --name "$candidate" \
  --env-file "$env_file" \
  --env SERVER_PORT=8080 \
  --env SERVER_MODE=release \
  --env REDIS_ADDR=wanxiang-redis:6379 \
  --network config_default \
  --read-only \
  --tmpfs /tmp \
  --cap-drop ALL \
  --security-opt no-new-privileges \
  "$image" >/dev/null

attempt=0
while [ "$attempt" -lt 18 ]; do
  health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$candidate" 2>/dev/null || true)
  case "$health" in
    healthy)
      break
      ;;
    unhealthy|exited|dead)
      docker logs --tail 100 "$candidate" >&2 || true
      echo "candidate failed health check: $health" >&2
      exit 1
      ;;
  esac
  attempt=$((attempt + 1))
  sleep 5
done

if [ "${health:-}" != "healthy" ]; then
	docker logs --tail 100 "$candidate" >&2 || true
	echo "candidate did not become healthy in time" >&2
	exit 1
fi

if ! docker exec "$candidate" wget -q -O /dev/null 'http://127.0.0.1:8080/api/v1/products?page=1&page_size=1'; then
	docker logs --tail 100 "$candidate" >&2 || true
	echo "candidate product API check failed" >&2
	exit 1
fi

cleanup
trap - EXIT INT TERM

previous_image=$(docker inspect -f '{{.Config.Image}}' wanxiang-backend 2>/dev/null || true)

rollback() {
  if [ -n "$previous_image" ] && docker image inspect "$previous_image" >/dev/null 2>&1; then
    previous_tag=${previous_image#wanxiang-backend:}
    echo "deployment failed; rolling back to $previous_tag" >&2
    IMAGE_TAG="$previous_tag" docker compose -f "$compose_file" up -d --remove-orphans --wait --wait-timeout 120
  else
    IMAGE_TAG="$release_tag" docker compose -f "$compose_file" down >/dev/null 2>&1 || true
  fi
}

if ! IMAGE_TAG="$release_tag" docker compose -f "$compose_file" up -d --remove-orphans --wait --wait-timeout 120; then
  rollback
  exit 1
fi

if ! docker exec wanxiang-backend wget -q -O /dev/null http://127.0.0.1:8080/health; then
	docker logs --tail 100 wanxiang-backend >&2 || true
	rollback
	exit 1
fi

if ! docker exec wanxiang-backend wget -q -O /dev/null 'http://127.0.0.1:8080/api/v1/products?page=1&page_size=1'; then
	docker logs --tail 100 wanxiang-backend >&2 || true
	rollback
	exit 1
fi

printf '%s\n' "$release_tag" > "$deploy_dir/current-release"
echo "wanxiang-backend deployed: $release_tag"
