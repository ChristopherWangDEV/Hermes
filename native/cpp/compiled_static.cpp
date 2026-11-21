// compiled_static.cpp —— 编译型产物的最后一道闸（与 cmd_compile.go 的
// checkCompiledStatic 一致）：非递归函数必须已经在 codegen 展开。
//
// 为什么这是闸门而不是优化：产物里若还留下指向非递归函数的 call_fn，运行期就会
// 变成动态派发 —— 而"Hermes 的边界在编译期"是这门语言的定义。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstdio>
#include <cstring>
#include <map>
#include <string>

extern "C" int hm_check_compiled_static(const char* ir_json, size_t len, char* out_msg, size_t cap,
                                        size_t* out_len) {
  if (ir_json == nullptr || out_msg == nullptr || out_len == nullptr) return -1;
  hm::JVal doc;
  size_t pos = 0;
  if (!hm::parse_json(ir_json, len, &doc, &pos) || doc.kind() != hm::JVal::kObj) return -1;

  std::map<std::string, bool> recursive;
  const hm::JVal* fns = doc.find("functions");
  if (fns != nullptr && fns->kind() == hm::JVal::kArr) {
    for (size_t i = 0; i < fns->size(); ++i) {
      const hm::JVal* f = fns->at(i);
      if (f == nullptr || f->kind() != hm::JVal::kObj) continue;
      const hm::JVal* n = f->find("name");
      const hm::JVal* r = f->find("recursive");
      if (n == nullptr || n->kind() != hm::JVal::kStr) continue;
      recursive[n->as_str()] = (r != nullptr && r->kind() == hm::JVal::kBool && r->as_bool(false));
    }
  }

  auto emit = [&](const std::string& msg) {
    const size_t n = msg.size() < cap - 1 ? msg.size() : cap - 1;
    std::memcpy(out_msg, msg.c_str(), n);
    out_msg[n] = 0;
    if (out_len != nullptr) *out_len = n + 1;
    return 1;
  };

  auto scan = [&](const char* where, const hm::JVal* nodes) -> int {
    if (nodes == nullptr || nodes->kind() != hm::JVal::kArr) return 0;
    for (size_t i = 0; i < nodes->size(); ++i) {
      const hm::JVal* n = nodes->at(i);
      if (n == nullptr || n->kind() != hm::JVal::kObj) continue;
      const hm::JVal* k = n->find("k");
      if (k == nullptr || k->kind() != hm::JVal::kStr || k->as_str() != "call_fn") continue;
      const hm::JVal* fn = n->find("fn");
      if (fn == nullptr || fn->kind() != hm::JVal::kStr) continue;
      const std::string name = fn->as_str();
      auto it = recursive.find(name);
      const bool rec = (it != recursive.end()) && it->second;
      if (!rec) {
        char buf[1024];
        std::snprintf(buf, sizeof buf,
                      "%s: call to non-recursive function \"%s\" survived codegen (compiled mode "
                      "must inline it); this is a compiler bug",
                      where, name.c_str());
        return emit(buf);
      }
    }
    return 0;
  };

  const hm::JVal* wfs = doc.find("workflows");
  if (wfs != nullptr && wfs->kind() == hm::JVal::kArr) {
    for (size_t i = 0; i < wfs->size(); ++i) {
      const hm::JVal* w = wfs->at(i);
      if (w == nullptr) continue;
      const hm::JVal* nm = w->find("name");
      const std::string where = "workflow " + std::string((nm != nullptr && nm->kind() == hm::JVal::kStr) ? nm->as_str() : "");
      if (int rc = scan(where.c_str(), w->find("nodes"))) return rc;
    }
  }
  if (fns != nullptr && fns->kind() == hm::JVal::kArr) {
    for (size_t i = 0; i < fns->size(); ++i) {
      const hm::JVal* f = fns->at(i);
      if (f == nullptr) continue;
      const hm::JVal* nm = f->find("name");
      const std::string where = "function " + std::string((nm != nullptr && nm->kind() == hm::JVal::kStr) ? nm->as_str() : "");
      if (int rc = scan(where.c_str(), f->find("nodes"))) return rc;
    }
  }
  return 0;
}
