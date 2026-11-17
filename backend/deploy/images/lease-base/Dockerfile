# 托管租约容器基础镜像 fleet.base_images[0](docs/25 §25.5.2)。开通时以
#   docker run … --entrypoint /bin/sh <镜像> -c '<containerInitScript>'
# 启动: 入口脚本写 authorized_keys(只有网关上游公钥, from=网关出口 IP)、生成 ed25519 主机密钥、exec /usr/sbin/sshd -D。
# 买家经网关以 root 登录, 得到 PyTorch 2.7.1 + CUDA 12.8 + Python 3 的开发环境。
# 与探针镜像共用 PyTorch 基础层。
FROM pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime@sha256:c16f4c749e2d9e96878875cdf6cc45cddda1d1a36fddd371dd6f2360f1b6e2a2

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server ca-certificates curl wget git vim-tiny less tmux htop rsync unzip iproute2 procps \
 && rm -rf /var/lib/apt/lists/* \
 # 主机密钥必须由每个容器在首次启动时各自生成(入口脚本 [ -s … ] || ssh-keygen): 镜像里若带密钥,
 # 所有租约容器共用同一把, 任一买家都能冒充其他租约的上游。
 && rm -f /etc/ssh/ssh_host_* \
 # sshd 不读 Docker 注入的环境变量: 写进 /etc/environment 让 ssh 登录后的 shell 也能找到 conda 的 python/pip 与 CUDA 库。
 && printf 'PATH="%s"\nLD_LIBRARY_PATH="%s"\nNVIDIA_DRIVER_CAPABILITIES="compute,utility"\n' \
      "/opt/conda/bin:/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
      "/usr/local/nvidia/lib:/usr/local/nvidia/lib64" > /etc/environment \
 && mkdir -p /root/workspace /run/sshd \
 && command -v sh sshd ssh-keygen python3 pip

WORKDIR /root/workspace
ENTRYPOINT []
CMD ["/bin/sh"]
LABEL org.opencontainers.image.source="https://github.com/NecoLiang/AI-Compute-Platform-" \
      org.opencontainers.image.description="omnisline fleet lease base image (PyTorch 2.7.1 CUDA 12.8 + openssh-server)"
