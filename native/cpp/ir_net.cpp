// ir_net.cpp —— 判定一份 IR 是否需要网络（`backend.UsesNet` 的 C++ 版）。
//
// 为什么它必须存在：`compile` 用它决定"要不要把 Go 网络归档链进产物"。
// 留在 Go 侧意味着 `compile` 永远离不开 `engine` 那一侧的实现——而网络运行时
// 是永久留 Go 的部分，编译链路不该被它拖住。
//
// 判据很窄：workflow 的**节点表**里出现 `call_tool` / `call_model` 就是需要。
// 只看节点 kind，不看语义——这一点与 Go 完全一致，因为两边都只该回答这一个问题。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstring>

extern "C" int hm_ir_uses_net(const char* ir_json, size_t len) {
  if (ir_json == nullptr || len == 0) return -1;
  hm::JVal doc;
  size_t pos = 0;
  if (!hm::parse_json(ir_json, len, &doc, &pos) || doc.kind() != hm::JVal::kObj) return -1;
  const hm::JVal* wfs = doc.find("workflows");
  if (wfs == nullptr || wfs->kind() != hm::JVal::kArr) return 0;
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
      if (kind == "call_tool" || kind == "call_model") return 1;
    }
  }
  return 0;
}
