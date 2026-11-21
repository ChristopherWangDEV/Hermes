// hostonly.cpp —— 裸机目标上的"宿主 OS 专属语义"闸门（与 toolchain.CheckHostOnlyNodes 一致）。
//
// 为什么需要：sleep / until / emit ... to 都依赖宿主 OS——前者要 nanosleep，
// 中者要条件变量与调度，后者要按上下文句柄做间接调用（裸机上没有这种句柄）。
// 它们没有诚实实现，只能在编译期拒绝。
//
// 为什么不检查 route == 1（频道路由）：频道是**编译期哈希 + 整数比较**，
// 没有任何串口/网络调用，裸机上照样能做。真正越界的是 route == 2（定向路由）。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstring>

namespace {
const char* kHostOnlyMessage =
    "[ERROR] Blocking synchronization or network routing requires a host OS.";
}

// 返回 0 = 通过，1 = 拒绝（out_msg 是定稿文案），-1 = 参数/JSON 非法。
extern "C" int hm_check_hostonly(const char* ir_json, size_t len, int bare_metal, char* out_msg,
                                 size_t cap, size_t* out_len) {
  if (ir_json == nullptr || out_msg == nullptr || out_len == nullptr) return -1;
  if (!bare_metal) return 0;
  hm::JVal doc;
  size_t pos = 0;
  if (!hm::parse_json(ir_json, len, &doc, &pos) || doc.kind() != hm::JVal::kObj) return -1;
  const hm::JVal* wfs = doc.find("workflows");
  if (wfs == nullptr || wfs->kind() != hm::JVal::kArr) return 0;

  auto hit = [&]() {
    const size_t n = std::strlen(kHostOnlyMessage);
    const size_t k = (n < cap - 1) ? n : cap - 1;
    std::memcpy(out_msg, kHostOnlyMessage, k);
    out_msg[k] = 0;
    *out_len = k + 1;
    return 1;
  };

  for (size_t i = 0; i < wfs->size(); ++i) {
    const hm::JVal* w = wfs->at(i);
    if (w == nullptr || w->kind() != hm::JVal::kObj) continue;
    const hm::JVal* nodes = w->find("nodes");
    if (nodes == nullptr || nodes->kind() != hm::JVal::kArr) continue;
    for (size_t j = 0; j < nodes->size(); ++j) {
      const hm::JVal* n = nodes->at(j);
      if (n == nullptr || n->kind() != hm::JVal::kObj) continue;
      const hm::JVal* k = n->find("k");
      if (k == nullptr || k->kind() != hm::JVal::kStr) continue;
      const std::string kind = k->as_str();
      if (kind == "sleep" || kind == "until") return hit();
      if (kind == "emit") {
        const hm::JVal* route = n->find("route");
        if (route != nullptr && route->kind() == hm::JVal::kInt && route->as_int(0) == 2) return hit();
      }
    }
  }
  return 0;
}
