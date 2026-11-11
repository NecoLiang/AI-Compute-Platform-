# 一键部署模型权重拉取镜像 fleet.model_pull_image(docs/27 §27.4.3)。节点上以
#   docker run -d … -v <缓存目录>:<缓存目录> --entrypoint sh <镜像> -c 'modelscope download … ; sha256sum -c …'
# 运行: 需要 sh、modelscope CLI(download --revision --local_dir)、sha256sum、cp/mv/rm。
# 与探针镜像共用 PyTorch 基础层(只多一层 modelscope)。
FROM pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime@sha256:c16f4c749e2d9e96878875cdf6cc45cddda1d1a36fddd371dd6f2360f1b6e2a2

RUN pip install --no-cache-dir modelscope==1.40.3 \
 && modelscope download --help >/dev/null \
 && command -v sh sha256sum cp mv rm

ENTRYPOINT []
CMD ["/bin/sh"]
LABEL org.opencontainers.image.source="https://github.com/NecoLiang/AI-Compute-Platform-" \
      org.opencontainers.image.description="omnisline fleet model pull image (modelscope CLI)"
