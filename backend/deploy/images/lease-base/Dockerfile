# 托管租约容器基础镜像 fleet.base_images[0](docs/25 §25.5.2)。开通时以
#   docker run … --entrypoint /bin/sh <镜像> -c '<containerInitScript>'
# 启动: 入口脚本写 authorized_keys、生成 ed25519 主机密钥、exec /usr/sbin/sshd -D。
# 买家以 root 登录, 得到 CUDA 12.8 运行时 + Python 3.10/pip/venv + 常用工具的精简环境(开通时现拉, 体积直接决定交付等待)。
# 不预装 PyTorch: pip install torch 自带对应 CUDA 库(nvidia-* wheel), 只依赖宿主机驱动; pip/apt 默认指向阿里云镜像。
# 与探针镜像、权重拉取镜像共用同一 CUDA 12.8 base 层。
FROM nvidia/cuda:12.8.1-base-ubuntu22.04@sha256:001469ea0f3dec85a1ca929aeea3b58ae369d4c11228b10aec1f642bb6ca7a6f

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server ca-certificates curl wget git vim-tiny less tmux htop rsync unzip iproute2 procps \
      python3 python3-pip python3-venv python-is-python3 \
 && rm -rf /var/lib/apt/lists/* \
 # 主机密钥必须由每个容器在首次启动时各自生成(入口脚本 [ -s … ] || ssh-keygen): 镜像里若带密钥,
 # 所有租约容器共用同一把, 任一买家都能冒充其他租约的上游。
 && rm -f /etc/ssh/ssh_host_* \
 && sed -i -e 's#http://archive.ubuntu.com/ubuntu#http://mirrors.aliyun.com/ubuntu#' \
      -e 's#http://security.ubuntu.com/ubuntu#http://mirrors.aliyun.com/ubuntu#' /etc/apt/sources.list \
 && printf '[global]\nindex-url = https://mirrors.aliyun.com/pypi/simple/\n' > /etc/pip.conf \
 # sshd 不读 Docker 注入的环境变量: 写进 /etc/environment 让 ssh 登录后的 shell 也能找到 CUDA 库与驱动工具。
 && printf 'PATH="%s"\nLD_LIBRARY_PATH="%s"\nNVIDIA_DRIVER_CAPABILITIES="compute,utility"\n' \
      "/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
      "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/cuda/lib64" > /etc/environment \
 && mkdir -p /root/workspace /run/sshd \
 && command -v sh sshd ssh-keygen python python3 pip

WORKDIR /root/workspace
ENTRYPOINT []
CMD ["/bin/sh"]
LABEL org.opencontainers.image.source="https://github.com/NecoLiang/AI-Compute-Platform-" \
      org.opencontainers.image.description="omnisline fleet lease base image (CUDA 12.8 + Python 3 + openssh-server)"
