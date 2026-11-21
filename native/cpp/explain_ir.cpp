// explain_ir.cpp —— `hermes explain <IR|.hbc>` 的**IR 契约**渲染（D-036 之外的 T36）。
//
// 输出与 cmd/hermes/cmd_explain.go 逐字一致：人读的表与 `--json` 两条路径都在这里。
// 为什么值得逐字：这是用户判断"这个 workflow 的上界是多少"的唯一入口，
// 数字旁的每个词都在解释那个数字能不能当承诺看。
#include "hermes_parser.h"
#include "json_write.h"

#include <algorithm>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

namespace {

std::string type_name(const hm::JVal* t) {
  if (t == nullptr || t->kind() != hm::JVal::kObj) return "unknown";
  const hm::JVal* k = t->find("k");
  std::string name = (k != nullptr && k->kind() == hm::JVal::kStr) ? k->as_str() : "unknown";
  if (name == "struct") {
    const hm::JVal* id = t->find("id");
    if (id != nullptr && id->kind() == hm::JVal::kInt) {
      char buf[32];
      std::snprintf(buf, sizeof buf, "struct#%lld", static_cast<long long>(id->as_int(0)));
      name = buf;
    }
  }
  const hm::JVal* of = t->find("of");
  if (of != nullptr && of->kind() == hm::JVal::kObj) name = name + "<" + type_name(of) + ">";
  return name;
}

std::string jstr(const hm::JVal* o, const char* k) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
}

long long jint(const hm::JVal* o, const char* k, long long dflt) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return (v != nullptr && v->kind() == hm::JVal::kInt) ? v->as_int(dflt) : dflt;
}

bool jbool(const hm::JVal* o, const char* k) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return v != nullptr && v->kind() == hm::JVal::kBool && v->as_bool(false);
}

// agent_steps_unbounded：节点里有 agent 且 max_steps 缺失或 ≤0。
//
// bounds.unbounded 只说"这几个数字不是上界"，不说明**为什么**；而
// max_steps = unlimited 的后果与"有个 while 循环"完全不同。机器要做判断就得
// 知道是哪一件，所以 JSON 里另给这个精确信号。
bool agent_steps_unbounded(const hm::JVal* wf) {
  const hm::JVal* nodes = wf->find("nodes");
  if (nodes == nullptr || nodes->kind() != hm::JVal::kArr) return false;
  for (size_t i = 0; i < nodes->size(); ++i) {
    const hm::JVal* n = nodes->at(i);
    if (n == nullptr) continue;
    if (jstr(n, "k") != "agent") continue;
    const hm::JVal* ag = n->find("agent");
    if (ag == nullptr || ag->kind() != hm::JVal::kObj) continue;
    const hm::JVal* ms = ag->find("max_steps");
    if (ms == nullptr || ms->kind() != hm::JVal::kInt || ms->as_int(0) <= 0) return true;
  }
  return false;
}


// go_value_str 复刻 Go 的 `%v`（只覆盖 economics 里会出现的形状）：
//   int → 十进制；string → 原样；bool → true/false；
//   object → `map[k:v ...]`（键排序，与 Go 的 map 打印一致）；array → `[a b c]`。
//
// 为什么值得逐字：economics 里是钱与 token 的上界。格式不同（比如把整数打成
// 1e+06）会让人以为上界是浮点，而那正是这条输出要避免的误解。
std::string go_value_str(const hm::JVal* v) {
  if (v == nullptr) return "<nil>";
  switch (v->kind()) {
    case hm::JVal::kInt:
      return std::to_string(static_cast<long long>(v->as_int(0)));
    case hm::JVal::kBool:
      return v->as_bool(false) ? "true" : "false";
    case hm::JVal::kStr:
      return v->as_str();
    case hm::JVal::kObj: {
      std::vector<std::string> keys;
      v->keys(&keys);
      std::sort(keys.begin(), keys.end());
      std::string out = "map[";
      for (size_t i = 0; i < keys.size(); ++i) {
        if (i) out += " ";
        out += keys[i] + ":" + go_value_str(v->find(keys[i]));
      }
      return out + "]";
    }
    case hm::JVal::kArr: {
      std::string out = "[";
      for (size_t i = 0; i < v->size(); ++i) {
        if (i) out += " ";
        out += go_value_str(v->at(i));
      }
      return out + "]";
    }
    default:
      return "<nil>";
  }
}

}  // namespace

// 返回 0 成功（out 是渲染结果）、1 找不到 workflow（out_err）、-1 参数非法、-4 缓冲不足。
extern "C" int hm_explain_ir(const char* ir_json, size_t len, const char* wf_name, int as_json,
                             const char* (*tr)(const char*), char* out, size_t cap, size_t* out_len,
                             char* out_err, size_t err_cap, size_t* err_len) {
  if (ir_json == nullptr || out == nullptr || out_len == nullptr) return -1;
  (void)err_len;
  hm::JVal doc;
  size_t pos = 0;
  if (!hm::parse_json(ir_json, len, &doc, &pos) || doc.kind() != hm::JVal::kObj) {
    return -1;
  }
  const hm::JVal* wfs = doc.find("workflows");
  if (wfs == nullptr || wfs->kind() != hm::JVal::kArr || wfs->size() == 0) {
    if (out_err != nullptr && err_cap > 0) std::snprintf(out_err, err_cap, "unit has no workflow");
    return 1;
  }
  const hm::JVal* wf = nullptr;
  const std::string want = (wf_name != nullptr) ? wf_name : "";
  if (want.empty()) {
    for (size_t i = 0; i < wfs->size(); ++i) {
      const hm::JVal* w = wfs->at(i);
      if (w != nullptr && jbool(w, "is_entry")) {
        wf = w;
        break;
      }
    }
    if (wf == nullptr) {
      if (out_err != nullptr && err_cap > 0) {
        std::snprintf(out_err, err_cap, "no entry workflow; use --workflow <name>");
      }
      return 1;
    }
  } else {
    std::vector<std::string> names;
    for (size_t i = 0; i < wfs->size(); ++i) {
      const hm::JVal* w = wfs->at(i);
      if (w == nullptr) continue;
      const std::string nm = jstr(w, "name");
      names.push_back(nm);
      if (nm == want) wf = w;
    }
    if (wf == nullptr) {
      std::sort(names.begin(), names.end());
      std::string joined;
      for (size_t i = 0; i < names.size(); ++i) {
        if (i) joined += ", ";
        joined += names[i];
      }
      if (out_err != nullptr && err_cap > 0) {
        std::snprintf(out_err, err_cap, "no workflow named \"%s\" (available: %s)", want.c_str(),
                      joined.c_str());
      }
      return 1;
    }
  }

  const hm::JVal* b = wf->find("bounds");
  const bool unbounded = jbool(b, "unbounded");
  const bool steps_unbounded = agent_steps_unbounded(wf);
  const hm::JVal* unit = doc.find("unit");
  const std::string unit_name = jstr(unit, "name");
  const hm::JVal* nodes = wf->find("nodes");
  const hm::JVal* vars = wf->find("vars");

  std::string o;
  char buf[256];
  if (as_json) {
    // **键必须按字典序**：Go 的 json.MarshalIndent 对 map 就是排序输出，
    // 而 --json 是给 CI 与工具用的机器契约——顺序不同会让 diff 噪声掩盖真变化。
    o += "{\n";
    std::snprintf(buf, sizeof buf, "  \"agent_steps_unbounded\": %s,\n",
                  steps_unbounded ? "true" : "false");
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"is_entry\": %s,\n", jbool(wf, "is_entry") ? "true" : "false");
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"max_concurrency\": %lld,\n",
                  static_cast<long long>(jint(b, "max_concurrency", 0)));
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"max_model_calls\": %lld,\n",
                  static_cast<long long>(jint(b, "max_model_calls", 0)));
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"max_spans\": %lld,\n",
                  static_cast<long long>(jint(b, "max_spans", 0)));
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"max_steps\": %lld,\n",
                  static_cast<long long>(jint(b, "max_steps", 0)));
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"max_tool_calls\": %lld,\n",
                  static_cast<long long>(jint(b, "max_tool_calls", 0)));
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"nodes\": %zu,\n", nodes != nullptr ? nodes->size() : 0);
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"unbounded\": %s,\n", unbounded ? "true" : "false");
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"vars\": %zu,\n", vars != nullptr ? vars->size() : 0);
    o += buf;
    std::snprintf(buf, sizeof buf, "  \"workflow\": \"%s\"\n}\n", jstr(wf, "name").c_str());
    o += buf;
  } else {
    // 人读：名字 + 标签（[entry exported unit=X]），然后逐字段。
    // 标签的顺序与 Go 一致；`unit=` 只在 unit 名非空时出现。
    std::string tags;
    if (jbool(wf, "is_entry")) tags += "entry";
    if (jbool(wf, "exported")) {
      if (!tags.empty()) tags += " ";
      tags += "exported";
    }
    if (!unit_name.empty()) {
      if (!tags.empty()) tags += " ";
      tags += "unit=" + unit_name;
    }
    std::snprintf(buf, sizeof buf, "%s  [%s]\n", jstr(wf, "name").c_str(), tags.c_str());
    o += buf;
    const hm::JVal* ret = wf->find("returns");
    std::snprintf(buf, sizeof buf, "  %-18s %s\n", "returns", type_name(ret).c_str());
    o += buf;
    const hm::JVal* input = wf->find("input");
    if (input != nullptr && input->kind() == hm::JVal::kObj) {
      std::snprintf(buf, sizeof buf, "  %-18s %s\n", "input", type_name(input).c_str());
      o += buf;
    }
    const std::string backend = jstr(wf, "backend");
    std::snprintf(buf, sizeof buf, "  %-18s %s\n", "backend",
                  backend.empty() ? "-" : backend.c_str());
    o += buf;
    std::snprintf(buf, sizeof buf, "  %-18s %zu\n", "nodes", nodes != nullptr ? nodes->size() : 0);
    o += buf;
    std::snprintf(buf, sizeof buf, "  %-18s %zu\n", "vars", vars != nullptr ? vars->size() : 0);
    o += buf;
    o += std::string("  ") + (tr ? tr("bounds (upper bounds, not expectations):")
                                 : "bounds (upper bounds, not expectations):") + "\n";
    if (unbounded) {
      std::snprintf(buf, sizeof buf, "    %-16s %s\n", "unbounded",
                    tr ? tr("yes (cost not statically bounded: a loop, a recursive call, or max_steps = unlimited)")
                       : "yes (cost not statically bounded: a loop, a recursive call, or max_steps = "
                         "unlimited)");
      o += buf;
    }
    if (steps_unbounded) {
      std::snprintf(buf, sizeof buf, "    %-16s %s\n", "max_steps",
                    tr ? tr("unlimited (no compile-time bound, and no runtime step cap either; stopping is left to cancellation, cost budgets and the process limits)")
                       : "unlimited (no compile-time bound, and no runtime step cap either; stopping "
                         "is left to cancellation, cost budgets and the process limits)");
      o += buf;
    } else if (unbounded) {
      std::snprintf(buf, sizeof buf, "    %-16s %s\n", "max_steps", tr ? tr("unknown") : "unknown");
      o += buf;
    } else {
      std::snprintf(buf, sizeof buf, "    %-16s %lld\n", "max_steps",
                    static_cast<long long>(jint(b, "max_steps", 0)));
      o += buf;
    }
    if (unbounded) {
      std::snprintf(buf, sizeof buf, "    %-16s %s\n", "max_spans", tr ? tr("unknown") : "unknown");
      o += buf;
    } else {
      std::snprintf(buf, sizeof buf, "    %-16s %lld\n", "max_spans",
                    static_cast<long long>(jint(b, "max_spans", 0)));
      o += buf;
    }
    std::snprintf(buf, sizeof buf, "    %-16s %lld\n", "max_concurrency",
                  static_cast<long long>(jint(b, "max_concurrency", 0)));
    o += buf;
    std::snprintf(buf, sizeof buf, "    %-16s %lld\n", "max_model_calls",
                  static_cast<long long>(jint(b, "max_model_calls", 0)));
    o += buf;
    std::snprintf(buf, sizeof buf, "    %-16s %lld\n", "max_tool_calls",
                  static_cast<long long>(jint(b, "max_tool_calls", 0)));
    o += buf;

    // economics：只在非空时打印。键排序输出（Go 的 map 打印也是排序的）。
    const hm::JVal* ec = wf->find("economics");
    if (ec != nullptr && ec->kind() == hm::JVal::kObj && !ec->empty_obj()) {
      o += "  economics:\n";
      std::vector<std::string> keys;
      ec->keys(&keys);
      std::sort(keys.begin(), keys.end());
      for (const std::string& k : keys) {
        const std::string val = go_value_str(ec->find(k));
        std::snprintf(buf, sizeof buf, "    %-16s %s\n", k.c_str(), val.c_str());
        o += buf;
      }
    }
  }

  const size_t need = o.size() + 1;
  if (need > cap) {
    *out_len = need;
    return -4;
  }
  std::memcpy(out, o.data(), o.size());
  out[o.size()] = 0;
  *out_len = need;
  return 0;
}
