# 访问网关镜像(docs/25 §25.6): sshpiperd v1.6.1 官方发布二进制(sha256 校验) + failtoban 插件 + 本仓库 cmd/accessgw 插件。
# 构建上下文为 backend/:  docker build -f deploy/accessgw/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/accessgw ./cmd/accessgw

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc AS sshpiperd
ARG TARGETARCH
ARG SSHPIPER_VERSION=v1.6.1
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) arch=x86_64; sum=95d423a70e843a7512a72fbb16c0fa5dc59217cf064cb3e277520fddffded1e1 ;; \
      arm64) arch=arm64;  sum=ceb92a4b91e8d163c3dafc3d40bc678a32046f076a557ea5b83ff573cf932c6f ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    wget -q -O /tmp/sp.tgz "https://github.com/tg123/sshpiper/releases/download/${SSHPIPER_VERSION}/sshpiperd_with_plugins_linux_${arch}.tar.gz"; \
    echo "$sum  /tmp/sp.tgz" | sha256sum -c -; \
    mkdir /sp && tar -xzf /tmp/sp.tgz -C /sp sshpiperd plugins/failtoban

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10022 accessgw
COPY --from=sshpiperd /sp/sshpiperd /usr/local/bin/sshpiperd
COPY --from=sshpiperd /sp/plugins/failtoban /usr/local/lib/sshpiperd/failtoban
COPY --from=build /out/accessgw /usr/local/bin/accessgw
# 密钥目录 /etc/omnis/accessgw 由宿主机只读挂载(host_ed25519、upstream_ed25519 属主 10022、权限 0400)。
USER 10022:10022
EXPOSE 2222
ENTRYPOINT ["/usr/local/bin/sshpiperd"]
LABEL org.opencontainers.image.source="https://github.com/NecoLiang/AI-Compute-Platform-" \
      org.opencontainers.image.description="omnisline access gateway (sshpiperd v1.6.1 + omnis-accessgw plugin)"
