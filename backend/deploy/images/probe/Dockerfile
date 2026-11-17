# 平台探针镜像 fleet.probe_image(docs/25 §25.4.3、§25.12)。按 digest 写入配置, 节点上的用途:
#   P9  docker run --gpus <空闲卡> <镜像> nvidia-smi -L        (nvidia-smi 由 NVIDIA 容器运行时注入)
#   P10 docker run --storage-opt size=64M <镜像> sh -c 'dd …'  (sh、dd)
#   P11 docker run -p <端口>:8000 <镜像> python3 -c …          (python3 临时监听)
#   P13 docker run --gpus <空闲卡> <镜像> python3 -c … <秒>     (CUDA 版 torch, FP16 矩阵乘)
#   开通② --network host --cap-add NET_ADMIN --entrypoint /bin/sh   (iptables-nft/iptables-legacy、base64、grep、sed)
#   整机   --privileged -v /:/host --entrypoint chroot               (chroot)
#   挖矿扫描 --network container:<租约> --entrypoint cat            (cat /proc/net/tcp)
# 与 lease-base、model-pull 共用同一 PyTorch 基础层, 节点拉过探针镜像后其余两个镜像只需拉增量层。
# torch 2.7.1 + CUDA 12.8: 覆盖 Ampere/Hopper/Ada/Blackwell; 驱动按 CUDA 12 小版本兼容, fleet.min_driver_version 见 README。
FROM pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime@sha256:c16f4c749e2d9e96878875cdf6cc45cddda1d1a36fddd371dd6f2360f1b6e2a2

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends iptables \
 && rm -rf /var/lib/apt/lists/* \
 && command -v iptables-nft iptables-legacy python3 sh base64 dd cat chroot grep sed tail

# 命令模板直接把命令接在镜像名之后(无 --entrypoint), 镜像不能带会改写参数或打印横幅的入口脚本。
ENTRYPOINT []
CMD ["/bin/sh"]
LABEL org.opencontainers.image.source="https://github.com/NecoLiang/AI-Compute-Platform-" \
      org.opencontainers.image.description="omnisline fleet probe image (python3 + CUDA torch + iptables)"
