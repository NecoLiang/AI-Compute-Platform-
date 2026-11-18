// omnis-bench: P13 GPU 压测(docs/25 §25.4.3), 探针镜像内 /usr/local/bin/omnis-bench <秒>。
// 每张可见卡一个线程, cuBLAS FP16 GEMM(FP32 累加, 与 torch 的 fp16 a @ b 同一路径)8192² 持续指定秒数,
// stdout 输出一行 {"results":[{"index","uuid","ok","tflops","error"}]}(解析见 internal/fleet/parse.go ParseBench)。
// UUID 取自 cudaDeviceProp.uuid, 格式与 nvidia-smi 的 GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx 一致, 不依赖枚举顺序。
// 无可用 CUDA 设备时输出 {"results":[]} 并把原因写到 stderr(平台按"压测无结果"判卡)。
#include <cublas_v2.h>
#include <cuda_fp16.h>
#include <cuda_runtime.h>

#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

namespace {

constexpr int kN = 8192;
constexpr size_t kElems = size_t(kN) * kN;

struct Result {
  int index = 0;
  std::string uuid;
  bool ok = false;
  double tflops = 0;
  std::string error;
};

void check(cudaError_t e, const char* what) {
  if (e != cudaSuccess) throw std::runtime_error(std::string(what) + ": " + cudaGetErrorString(e));
}

void check(cublasStatus_t s, const char* what) {
  if (s != CUBLAS_STATUS_SUCCESS) throw std::runtime_error(std::string(what) + ": " + cublasGetStatusString(s));
}

std::string uuid_of(const cudaDeviceProp& p) {
  const auto* b = reinterpret_cast<const unsigned char*>(p.uuid.bytes);
  char buf[48];
  snprintf(buf, sizeof buf, "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x", b[0], b[1], b[2], b[3],
           b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15]);
  return buf;
}

std::string json_str(const std::string& s) {
  std::string out = "\"";
  for (unsigned char c : s) {
    if (c == '"' || c == '\\') {
      out += '\\';
      out += char(c);
    } else if (c < 0x20) {
      char buf[8];
      snprintf(buf, sizeof buf, "\\u%04x", c);
      out += buf;
    } else {
      out += char(c);
    }
  }
  return out + "\"";
}

// 均匀分布于 [-1, 1) 的 FP16 矩阵(固定种子, 各卡相同输入)。
std::vector<__half> random_matrix(uint64_t seed) {
  std::vector<__half> m(kElems);
  uint64_t x = seed;
  for (auto& v : m) {
    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    v = __float2half(float(x >> 40) / float(1 << 23) - 1.0f);
  }
  return m;
}

void run(int dev, double secs, const std::vector<__half>& ha, const std::vector<__half>& hb, Result& r) {
  r.index = dev;
  __half *a = nullptr, *b = nullptr, *c = nullptr;
  cublasHandle_t h = nullptr;
  try {
    check(cudaSetDevice(dev), "cudaSetDevice");
    cudaDeviceProp p;
    check(cudaGetDeviceProperties(&p, dev), "cudaGetDeviceProperties");
    r.uuid = uuid_of(p);
    const size_t bytes = kElems * sizeof(__half);
    check(cudaMalloc(&a, bytes), "cudaMalloc");
    check(cudaMalloc(&b, bytes), "cudaMalloc");
    check(cudaMalloc(&c, bytes), "cudaMalloc");
    check(cudaMemcpy(a, ha.data(), bytes, cudaMemcpyHostToDevice), "cudaMemcpy");
    check(cudaMemcpy(b, hb.data(), bytes, cudaMemcpyHostToDevice), "cudaMemcpy");
    check(cublasCreate(&h), "cublasCreate");
    const float alpha = 1.0f, beta = 0.0f;
    auto gemm = [&] {
      check(cublasGemmEx(h, CUBLAS_OP_N, CUBLAS_OP_N, kN, kN, kN, &alpha, a, CUDA_R_16F, kN, b, CUDA_R_16F, kN, &beta, c,
                         CUDA_R_16F, kN, CUBLAS_COMPUTE_32F, CUBLAS_GEMM_DEFAULT),
            "cublasGemmEx");
    };
    for (int i = 0; i < 3; i++) gemm();
    check(cudaDeviceSynchronize(), "cudaDeviceSynchronize");
    using clock = std::chrono::steady_clock;
    long it = 0;
    const auto t0 = clock::now();
    double dt = 0;
    while (dt < secs) {
      for (int i = 0; i < 10; i++) gemm();
      check(cudaDeviceSynchronize(), "cudaDeviceSynchronize");
      it += 10;
      dt = std::chrono::duration<double>(clock::now() - t0).count();
    }
    std::vector<uint16_t> hc(kElems);
    check(cudaMemcpy(hc.data(), c, bytes, cudaMemcpyDeviceToHost), "cudaMemcpy");
    for (uint16_t v : hc) {
      if ((v & 0x7C00) == 0x7C00) throw std::runtime_error("non-finite result");  // FP16 指数全 1: Inf/NaN
    }
    r.tflops = std::round(2.0 * double(kN) * kN * kN * double(it) / dt / 1e12 * 10) / 10;
    r.ok = true;
  } catch (const std::exception& e) {
    r.error = std::string(e.what()).substr(0, 200);
  }
  if (h) cublasDestroy(h);
  for (__half* m : {a, b, c}) {
    if (m) cudaFree(m);
  }
}

}  // namespace

int main(int argc, char** argv) {
  double secs = 60;
  if (argc > 1) {
    char* end = nullptr;
    secs = strtod(argv[1], &end);
    if (end == argv[1] || *end != '\0' || !(secs > 0) || secs > 3600) {
      fprintf(stderr, "usage: omnis-bench <seconds>\n");
      return 2;
    }
  }
  int n = 0;
  cudaError_t e = cudaGetDeviceCount(&n);
  if (e != cudaSuccess) {
    fprintf(stderr, "omnis-bench: cudaGetDeviceCount: %s\n", cudaGetErrorString(e));
    n = 0;
  }
  std::vector<Result> res(n);
  if (n > 0) {
    const auto ha = random_matrix(0x9E3779B97F4A7C15ull), hb = random_matrix(0xD1B54A32D192ED03ull);
    std::vector<std::thread> ts;
    for (int i = 0; i < n; i++) ts.emplace_back(run, i, secs, std::cref(ha), std::cref(hb), std::ref(res[i]));
    for (auto& t : ts) t.join();
  }
  std::string out = "{\"results\":[";
  for (size_t i = 0; i < res.size(); i++) {
    const Result& r = res[i];
    char num[32];
    snprintf(num, sizeof num, "%.1f", r.tflops);
    if (i) out += ",";
    out += "{\"index\":" + std::to_string(r.index) + ",\"uuid\":" + json_str(r.uuid) + ",\"ok\":" + (r.ok ? "true" : "false") +
           ",\"tflops\":" + num + ",\"error\":" + json_str(r.error) + "}";
  }
  out += "]}";
  puts(out.c_str());
  return 0;
}
