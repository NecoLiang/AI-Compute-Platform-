#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "bootstrap-server.sh must run as root" >&2
  exit 1
fi

middleware_env=${MIDDLEWARE_ENV_FILE:-/opt/wanxiang/config/.env.middleware}
deploy_dir=${BACKEND_DEPLOY_DIR:-/opt/wanxiang/backend}
backend_env="$deploy_dir/.env"

if [ ! -s "$middleware_env" ]; then
  echo "middleware environment is missing: $middleware_env" >&2
  exit 1
fi

if [ -e "$backend_env" ]; then
  echo "refusing to replace existing backend environment: $backend_env" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
. "$middleware_env"
set +a

require_value() {
  if [ -z "$1" ]; then
    echo "required middleware variable is missing: $2" >&2
    exit 1
  fi
}

require_value "${MYSQL_DATABASE:-}" MYSQL_DATABASE
require_value "${MYSQL_USER:-}" MYSQL_USER
require_value "${MYSQL_PASSWORD:-}" MYSQL_PASSWORD
require_value "${REDIS_PASSWORD:-}" REDIS_PASSWORD
require_value "${CREDENTIAL_KEY:-}" CREDENTIAL_KEY

if [ "${#CREDENTIAL_KEY}" -ne 64 ]; then
  echo "CREDENTIAL_KEY must be 64 hexadecimal characters" >&2
  exit 1
fi
case "$CREDENTIAL_KEY" in
  *[!0-9a-fA-F]*)
    echo "CREDENTIAL_KEY must be 64 hexadecimal characters" >&2
    exit 1
    ;;
esac

install -d -m 700 "$deploy_dir"
umask 077
access_secret=$(openssl rand -hex 32)
refresh_secret=$(openssl rand -hex 32)
temp_env=$(mktemp "$deploy_dir/.env.tmp.XXXXXX")

cleanup() {
  rm -f "$temp_env"
}
trap cleanup EXIT INT TERM

printf '%s\n' \
  "DATABASE_DSN=${MYSQL_USER}:${MYSQL_PASSWORD}@tcp(wanxiang-mysql:3306)/${MYSQL_DATABASE}?parseTime=true&charset=utf8mb4" \
  "REDIS_PASSWORD=${REDIS_PASSWORD}" \
  "REDIS_DB=0" \
  "JWT_ACCESS_SECRET=${access_secret}" \
  "JWT_REFRESH_SECRET=${refresh_secret}" \
  "JWT_ACCESS_TTL=900" \
  "JWT_REFRESH_TTL=604800" \
  "SECURITY_CREDENTIAL_KEY=${CREDENTIAL_KEY}" \
  > "$temp_env"

chmod 600 "$temp_env"
mv "$temp_env" "$backend_env"
trap - EXIT INT TERM

echo "backend environment created: $backend_env"
