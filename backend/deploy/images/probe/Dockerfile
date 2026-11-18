# 平台探针镜像 fleet.probe_image(docs/25 §25.4.3、§25.12)。按 digest 写入配置, 节点上的用途:
#   P9  docker run --gpus <空闲卡> <镜像> nvidia-smi -L        (nvidia-smi 由 NVIDIA 容器运行时注入)
#   P10 docker run --storage-opt size=64M <镜像> sh -c 'dd …'  (sh、dd)
#   P11 docker run -p <端口>:8000 <镜像> python3 -c …          (python3 临时监听)
#   P13 docker run --gpus <空闲卡> <镜像> omnis-bench <秒>      (bench.cpp: cuBLAS FP16 矩阵乘, 与 torch fp16 matmul 同口径)
#   开通② --network host --cap-add NET_ADMIN --entrypoint /bin/sh   (iptables-nft/iptables-legacy、base64、grep、sed)
#   整机   --privileged -v /:/host --entrypoint chroot               (chroot)
#   挖矿扫描 --network container:<租约> --entrypoint cat            (cat /proc/net/tcp)
# 不再带 torch: 压测只需要 cuBLAS 运行库, 镜像从 ~4GB 降到 cuBLAS 为主的体积。
# 与 lease-base、model-pull 共用同一 CUDA 12.8 base 层; 驱动须 >= 570(CUDA 12.8), fleet.min_driver_version 见 README。
ARG BASE=nvidia/cuda:12.8.1-base-ubuntu22.04@sha256:001469ea0f3dec85a1ca929aeea3b58ae369d4c11228b10aec1f642bb6ca7a6f

# cuda-nvcc 只为 crt/host_defines.h 等运行时头文件(主机代码用 g++ 编译, 不含设备代码)。
FROM ${BASE} AS bench
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends g++ cuda-cudart-dev-12-8 cuda-nvcc-12-8 libcublas-dev-12-8 \
 && rm -rf /var/lib/apt/lists/*
COPY bench.cpp /src/bench.cpp
RUN mkdir -p /out \
 && g++ -O2 -std=c++17 -Wall -Wextra -Werror -I/usr/local/cuda/include /src/bench.cpp -o /out/omnis-bench \
      -L/usr/local/cuda/lib64 -lcublas -lcudart -pthread

FROM ${BASE}
# libcublas-12-8 与构建阶段的 libcublas-dev-12-8 在同一次构建中解析到同一版本(dev 依赖同版本运行库)。
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends iptables python3 libcublas-12-8 \
 && rm -rf /var/lib/apt/lists/* \
 && command -v iptables-nft iptables-legacy python3 sh base64 dd cat chroot grep sed tail
COPY --from=bench /out/omnis-bench /usr/local/bin/omnis-bench
# 构建机无 GPU: 动态库全部可解析, 且按约定输出空结果。
RUN ! ldd /usr/local/bin/omnis-bench | grep 'not found' \
 && omnis-bench 5 | grep -qx '{"results":\[\]}'

# 命令模板直接把命令接在镜像名之后(无 --entrypoint), 镜像不能带会改写参数或打印横幅的入口脚本。
ENTRYPOINT []
CMD ["/bin/sh"]
LABEL org.opencontainers.image.source="https://github.com/NecoLiang/AI-Compute-Platform-" \
      org.opencontainers.image.description="omnisline fleet probe image (python3 + cuBLAS bench + iptables)"
