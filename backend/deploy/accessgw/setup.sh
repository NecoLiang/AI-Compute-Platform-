#!/usr/bin/env bash
# 访问网关与主服务同机部署的一次性准备(可重复执行, 已存在的项只校验不覆盖)。拓扑见 compose.yml 头注释与 README.md。
#
#   setup.sh --bind-ip <辅助私网IP> --egress-ip <辅助私网IP绑定的EIP> --public-host <网关域名> --image <网关镜像@sha256> \
#            [--add-secondary-ip] [--up]
#
# 做的事:
#   1. 校验辅助私网 IP 已在 eth0 上(--add-secondary-ip 时经 NetworkManager 持久添加, 须先在控制台把该 IP 分配给主网卡)
#   2. 校验主服务已创建 omnis-accessgw-internal; 创建 omnis-accessgw-egress(出口 SNAT 到辅助私网 IP)
#   3. 实测网关出口公网地址 == --egress-ip(否则节点防火墙放行的不是网关, P12 也无从谈起)
#   4. 生成网关主机密钥/上游密钥(属主 10022, 0400)、主服务自检私钥(root, 0400)、HMAC 共享密钥
#   5. 写网关 .env 与主服务 .env 中缺少的 ACCESSGW_* / FLEET_SELFTEST_KEY_FILE(已有值不同则报错退出, 主服务 .env 先备份)
#   6. --up 时 docker compose up -d 并检查 2222 端口
# 不改 fleet.enabled; 私钥与 HMAC 不输出, 只打印公钥与指纹。
set -euo pipefail

gw_dir=/opt/omnis-accessgw
backend_dir=/opt/wanxiang/backend
nm_con="cloud-init eth0"
dev=eth0
egress_net=omnis-accessgw-egress
egress_subnet=172.30.20.0/24
internal_net=omnis-accessgw-internal
gw_uid=10022
selftest_key=/etc/omnis/selftest_ed25519 # 容器内路径, 宿主机为 $backend_dir/secrets/selftest_ed25519

bind_ip='' egress_ip='' public_host='' image='' add_ip=0 up=0
while [ $# -gt 0 ]; do
  case "$1" in
    --bind-ip) bind_ip=${2:-}; shift 2 ;;
    --egress-ip) egress_ip=${2:-}; shift 2 ;;
    --public-host) public_host=${2:-}; shift 2 ;;
    --image) image=${2:-}; shift 2 ;;
    --add-secondary-ip) add_ip=1; shift ;;
    --up) up=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

die() { echo "ERROR: $*" >&2; exit 1; }
ipv4_re='^([0-9]{1,3}\.){3}[0-9]{1,3}$'
[ "$(id -u)" = 0 ] || die "must run as root"
[[ $bind_ip =~ $ipv4_re ]] || die "--bind-ip <辅助私网 IPv4> is required"
[[ $egress_ip =~ $ipv4_re ]] || die "--egress-ip <辅助私网 IP 绑定的 EIP> is required"
[[ $public_host =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] ||
  die "--public-host 须为小写域名(如 gw.omnisline.com), 解析到 --egress-ip"
[[ $image =~ @sha256:[0-9a-f]{64}$ ]] || die "--image 须按 digest 固定(…@sha256:<64hex>)"
for c in docker ssh-keygen openssl ip; do command -v "$c" >/dev/null || die "missing command: $c"; done
[ -f "$gw_dir/compose.yml" ] || die "$gw_dir/compose.yml not found (把 deploy/accessgw/compose.yml 与本脚本一起拷到 $gw_dir)"
[ -s "$backend_dir/.env" ] || die "$backend_dir/.env not found"

# 1. 辅助私网 IP
has_ip() { ip -4 -o addr show dev "$dev" | awk '{print $4}' | cut -d/ -f1 | grep -qx "$1"; }
if ! has_ip "$bind_ip"; then
  [ "$add_ip" = 1 ] || die "$bind_ip 不在 $dev 上。先在 ECS 控制台为主网卡分配该辅助私网 IP, 再加 --add-secondary-ip 重跑"
  prefix=$(ip -4 -o addr show dev "$dev" | awk 'NR==1{print $4}' | cut -d/ -f2)
  nmcli con mod "$nm_con" +ipv4.addresses "$bind_ip/$prefix"
  nmcli device reapply "$dev"
  for _ in 1 2 3 4 5; do has_ip "$bind_ip" && break; sleep 1; done
  has_ip "$bind_ip" || die "添加 $bind_ip/$prefix 后仍未生效"
  echo "added $bind_ip/$prefix to $dev (NetworkManager: $nm_con)"
fi

# 2. 网络
docker network inspect "$internal_net" >/dev/null 2>&1 ||
  die "$internal_net 不存在: 先部署包含 accessgw 网络的主服务版本(deploy/compose.yml 会创建它)"
[ "$(docker network inspect -f '{{.Internal}}' "$internal_net")" = true ] || die "$internal_net 必须是 internal 网络"
if docker network inspect "$egress_net" >/dev/null 2>&1; then
  got=$(docker network inspect -f '{{index .Options "com.docker.network.host_ipv4"}}' "$egress_net")
  [ "$got" = "$bind_ip" ] || die "$egress_net 已存在但出口 IP 为 '$got' 而非 $bind_ip; 确认无容器使用后 docker network rm $egress_net 再重跑"
else
  docker network create --driver bridge --subnet "$egress_subnet" \
    -o com.docker.network.host_ipv4="$bind_ip" --label omnis.role=accessgw-egress "$egress_net" >/dev/null
  echo "created $egress_net ($egress_subnet, SNAT -> $bind_ip)"
fi

# 3. 实测出口
docker pull -q "$image" >/dev/null
seen=
for url in https://api.ipify.org https://ifconfig.me/ip http://ip.3322.net; do
  seen=$(docker run --rm --network "$egress_net" --entrypoint wget "$image" -qO- -T 8 "$url" 2>/dev/null | tr -d '[:space:]') || true
  [[ $seen =~ $ipv4_re ]] && break
  seen=
done
[ -n "$seen" ] || die "无法从 $egress_net 访问外网取得出口地址(检查 EIP 绑定与安全组出方向)"
[ "$seen" = "$egress_ip" ] || die "网关出口公网地址为 $seen, 不是 --egress-ip $egress_ip(EIP 未绑定到 $bind_ip?)"
echo "egress verified: $egress_net -> $bind_ip -> $egress_ip"

# 4. 密钥(已存在不覆盖)
install -d -m 0755 "$gw_dir"
install -d -m 0500 -o "$gw_uid" -g "$gw_uid" "$gw_dir/keys"
for k in host_ed25519 upstream_ed25519; do
  f="$gw_dir/keys/$k"
  if [ ! -f "$f" ]; then
    ssh-keygen -q -t ed25519 -N '' -C "omnis-accessgw-${k%_ed25519}" -f "$f"
    echo "generated $f"
  fi
  chown "$gw_uid:$gw_uid" "$f" "$f.pub"
  chmod 0400 "$f"
  chmod 0444 "$f.pub"
done
install -d -m 0700 "$backend_dir/secrets"
st="$backend_dir/secrets/selftest_ed25519"
if [ ! -f "$st" ]; then
  ssh-keygen -q -t ed25519 -N '' -C omnis-selftest -f "$st"
  echo "generated $st"
fi
chown root:root "$st"
chmod 0400 "$st"

# 5. 环境文件
env_get() { grep -E "^$2=" "$1" 2>/dev/null | tail -n 1 | cut -d= -f2- || true; }
backup_done=0
# env_put <文件> <键> <值>: 缺少则追加; 已有相同值跳过; 不同则报错(不覆盖人工配置)。
env_put() {
  local file=$1 key=$2 val=$3 cur
  cur=$(env_get "$file" "$key")
  if [ -z "$cur" ]; then
    if [ "$file" = "$backend_dir/.env" ] && [ "$backup_done" = 0 ]; then
      cp -p "$file" "$file.before-accessgw-$(date -u +%Y%m%dT%H%M%SZ)"
      backup_done=1
    fi
    printf '%s=%s\n' "$key" "$val" >>"$file"
    echo "set $key in $file"
  elif [ "$cur" != "$val" ]; then
    die "$file 中 $key 已有不同的值; 确认后手工改正再重跑"
  fi
}
hmac=$(env_get "$backend_dir/.env" ACCESSGW_HMAC_SECRET)
[ -n "$hmac" ] || hmac=$(env_get "$gw_dir/.env" ACCESSGW_HMAC_SECRET)
[ -n "$hmac" ] || hmac=$(openssl rand -hex 32)
[ "${#hmac}" -ge 32 ] || die "现有 ACCESSGW_HMAC_SECRET 不足 32 字节"
upstream_pub=$(cut -d' ' -f1,2 "$gw_dir/keys/upstream_ed25519.pub")
host_fp=$(ssh-keygen -lf "$gw_dir/keys/host_ed25519.pub" | awk '{print $2}')

touch "$gw_dir/.env"
chmod 0600 "$gw_dir/.env"
env_put "$gw_dir/.env" ACCESSGW_IMAGE "$image"
env_put "$gw_dir/.env" ACCESSGW_BIND_IP "$bind_ip"
env_put "$gw_dir/.env" ACCESSGW_PUBLIC_HOST "$public_host"
env_put "$gw_dir/.env" ACCESSGW_HMAC_SECRET "$hmac"

env_put "$backend_dir/.env" ACCESSGW_PUBLIC_HOST "$public_host"
env_put "$backend_dir/.env" ACCESSGW_PUBLIC_PORT 2222
env_put "$backend_dir/.env" ACCESSGW_INTERNAL_LISTEN 0.0.0.0:9090
env_put "$backend_dir/.env" ACCESSGW_EGRESS_IPS "$egress_ip"
env_put "$backend_dir/.env" ACCESSGW_UPSTREAM_PUBKEY "$upstream_pub"
env_put "$backend_dir/.env" ACCESSGW_HOST_KEY_FINGERPRINT "$host_fp"
env_put "$backend_dir/.env" ACCESSGW_HMAC_SECRET "$hmac"
env_put "$backend_dir/.env" FLEET_SELFTEST_KEY_FILE "$selftest_key"
unset hmac

# 6. 启动
if [ "$up" = 1 ]; then
  docker compose -f "$gw_dir/compose.yml" --project-directory "$gw_dir" up -d
  for _ in $(seq 1 15); do
    if banner=$(timeout 3 bash -c "exec 3<>/dev/tcp/$bind_ip/2222 && IFS= read -r l <&3 && printf %s \"\$l\"" 2>/dev/null) &&
      [[ $banner == SSH-2.0-* ]]; then
      break
    fi
    banner=
    sleep 1
  done
  [ -n "$banner" ] || die "网关未在 $bind_ip:2222 返回 SSH 握手; docker logs omnis-accessgw"
  echo "gateway listening on $bind_ip:2222 (${banner%%$'\r'*})"
fi

cat <<EOF

== 非敏感信息 ==
网关上游公钥(accessgw.upstream_pubkey): $upstream_pub
网关主机密钥指纹(accessgw.host_key_fingerprint): $host_fp
网关出口 IP(accessgw.egress_ips, 节点防火墙放行): $egress_ip
自检私钥: $st -> 容器内 $selftest_key

主服务 .env 的上述项在下次部署/重启主服务时生效; fleet.enabled 仍为原值。
打开 fleet 还需: FLEET_ENABLED=true FLEET_CONTROLLER_EGRESS_IPS FLEET_BASE_IMAGES FLEET_PROBE_IMAGE
FLEET_MIN_DRIVER_VERSION FLEET_MODEL_PULL_IMAGE FLEET_DENY_CIDRS(见 README.md)。
EOF
