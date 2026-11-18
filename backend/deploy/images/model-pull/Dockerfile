# 一键部署模型权重拉取镜像 fleet.model_pull_image(docs/27 §27.4.3)。节点上以
#   docker run -d … -v <缓存目录>:<缓存目录> --entrypoint sh <镜像> -c 'modelscope download … ; sha256sum -c …'
# 运行: 需要 sh、modelscope CLI(download --revision --local_dir)、sha256sum、cp/mv/rm。
# 不需要 GPU 库; 沿用与探针、租约镜像相同的 CUDA 12.8 base 层(节点已有), 只多 python3 + modelscope 一层。
FROM nvidia/cuda:12.8.1-base-ubuntu22.04@sha256:001469ea0f3dec85a1ca929aeea3b58ae369d4c11228b10aec1f642bb6ca7a6f

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends python3 python3-pip ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir modelscope==1.40.3 \
 && modelscope download --help >/dev/null \
 && command -v sh sha256sum cp mv rm

ENTRYPOINT []
CMD ["/bin/sh"]
LABEL org.opencontainers.image.source="https://github.com/NecoLiang/AI-Compute-Platform-" \
      org.opencontainers.image.description="omnisline fleet model pull image (modelscope CLI)"
