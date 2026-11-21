// importhomo.cpp —— 把 `homonoia compile --emit --json` 的产物转成 `.hermes` 源码。
//
// 边界（D-023 路 A）：输入是 **JSON 计划**（不是 Homonoia 的部署源文件），
// 输出是 **`.hermes` 源码**（不是旁路数据）。只认 `--http` 与 `--model` 两节，
// 其余一律丢弃并 warn——认不透就少导几项，绝不"猜着导"。
//
// 两次导入同一 plan 必须**逐字节相等**（无时间戳、warnings 排序稳定）。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstring>
#include <string>
#include <vector>

namespace {

struct Step {
  std::string kind;
  std::vector<std::string> argv;
  bool has_note = false;
  std::string note;
};

// one_line 把多行文本压成一行：注释里出现换行会把后面的内容踢出注释，
// 变成一行源码——那是能把导入产物变成非法文件的最简单方式。
std::string one_line(const std::string& s) {
  std::string out;
  out.reserve(s.size());
  for (char c : s) {
    if (c == '\r' || c == '\n') {
      out.push_back(' ');
    } else {
      out.push_back(c);
    }
  }
  // trim
  size_t b = 0, e = out.size();
  while (b < e && (out[b] == ' ' || out[b] == '\t')) ++b;
  while (e > b && (out[e - 1] == ' ' || out[e - 1] == '\t')) --e;
  return out.substr(b, e - b);
}

std::string escape_lit(const std::string& s) {
  std::string out;
  for (char c : s) {
    if (c == '\\' || c == '"') out.push_back('\\');
    out.push_back(c);
  }
  return out;
}

bool starts_with_dashes(const std::string& s) {
  return s.size() >= 2 && s[0] == '-' && s[1] == '-';
}

}  // namespace

// 返回 0 成功（out_src 是源码，out_warn 是 JSON 数组）、1 不是可用计划（out_err）、
// -1 参数非法、-4 缓冲不足（*out_len 给出所需大小）。
extern "C" int hm_import_plan(const char* json, size_t len, char* out_src, size_t cap,
                              size_t* out_len, char* out_warn, size_t warn_cap, size_t* warn_len,
                              char* out_err, size_t err_cap, size_t* err_len) {
  if (json == nullptr || out_src == nullptr || out_len == nullptr) return -1;
  auto set_err = [&](const std::string& m) {
    if (out_err != nullptr && err_cap > 0) {
      const size_t n = m.size() < err_cap - 1 ? m.size() : err_cap - 1;
      std::memcpy(out_err, m.c_str(), n);
      out_err[n] = 0;
    }
    if (err_len != nullptr) *err_len = m.size() + 1;
    return 1;
  };

  hm::JVal arr;
  size_t pos = 0;
  if (!hm::parse_json(json, len, &arr, &pos) || arr.kind() != hm::JVal::kArr) {
    return set_err("not the output of `homonoia compile --emit --json` (expected a JSON array)");
  }
  if (arr.size() == 0) return set_err("empty plan: no steps in the array, nothing to import");

  std::vector<Step> steps;
  steps.reserve(arr.size());
  for (size_t i = 0; i < arr.size(); ++i) {
    const hm::JVal* o = arr.at(i);
    if (o == nullptr || o->kind() != hm::JVal::kObj) {
      return set_err("step " + std::to_string(i) + " is not an object");
    }
    // DisallowUnknownFields 的等价物：上游加了字段而这边不认，静默忽略会让
    // "导入少了一半"看起来像"上游没写"。宁可报错让人来看一眼。
    std::vector<std::string> keys;
    o->keys(&keys);
    for (const std::string& key : keys) {
      if (key != "kind" && key != "argv" && key != "note" && key != "file") {
        return set_err("step " + std::to_string(i) + " has unknown field \"" + key + "\"");
      }
    }
    Step s;
    const hm::JVal* kind = o->find("kind");
    if (kind != nullptr && kind->kind() == hm::JVal::kStr) s.kind = kind->as_str();
    if (s.kind.empty()) return set_err("step " + std::to_string(i) + " has no kind");
    const hm::JVal* argv = o->find("argv");
    if (argv == nullptr || argv->kind() != hm::JVal::kArr) {
      return set_err("step " + std::to_string(i) + " (" + s.kind + ") has no argv");
    }
    for (size_t a = 0; a < argv->size(); ++a) {
      const hm::JVal* v = argv->at(a);
      if (v == nullptr || v->kind() != hm::JVal::kStr) {
        return set_err("step " + std::to_string(i) + " (" + s.kind + ") has a non-string argv entry");
      }
      s.argv.push_back(v->as_str());
    }
    if (s.argv.empty() || s.argv[0].empty()) {
      return set_err("step " + std::to_string(i) + " (" + s.kind + ") has no argv[0]");
    }
    const hm::JVal* note = o->find("note");
    if (note != nullptr && note->kind() == hm::JVal::kStr) {
      s.has_note = true;
      s.note = note->as_str();
    }
    steps.push_back(std::move(s));
  }

  // ---- 提取 --http / --model ----
  std::vector<std::string> warnings;
  std::vector<std::string> warned;
  auto warn_once = [&](const std::string& key, const std::string& msg) {
    for (const std::string& w : warned) {
      if (w == key) return;
    }
    warned.push_back(key);
    warnings.push_back(msg);
  };

  std::string http_addr, http_raw;
  std::vector<std::string> models;
  std::string default_model;

  for (const Step& s : steps) {
    for (size_t i = 0; i < s.argv.size(); ++i) {
      const std::string& arg = s.argv[i];
      if (!starts_with_dashes(arg)) continue;
      std::string val;
      const bool has_val = i + 1 < s.argv.size() && !starts_with_dashes(s.argv[i + 1]);
      if (has_val) val = s.argv[i + 1];
      if (arg != "--http" && arg != "--model") {
        warn_once(arg + "|" + s.kind,
                  "dropping unrecognized argument " + arg + " (from " + s.kind +
                      "): the importer only understands --http and --model, the meaning of the rest "
                      "belongs to Homonoia and is not reconstructed here");
        continue;
      }
      if (arg == "--http" && has_val && http_addr.empty()) {
        http_raw = val;
        // 补 scheme 只在缺的时候做：已经带 http(s):// 的照抄，
        // 免得把 `https://x` 变成 `http://https://x`。
        if (val.find("://") == std::string::npos) val = "http://" + val;
        http_addr = val;
      }
      if (arg == "--model" && has_val) {
        models.push_back(val);
        const size_t eq = val.find('=');
        if (eq != std::string::npos && default_model.empty()) default_model = val.substr(0, eq);
      }
      if (has_val) ++i;  // 跳过值，避免把 --model 的路径当成另一个 flag
    }
  }

  std::sort(warnings.begin(), warnings.end());  // 顺序稳定：两次导入的 warning 也要可比

  // ---- 生成源码 ----
  std::string b;
  b += "// Generated by `hermes import --from-plan`. Input is a Homonoia deployment plan.\n";
  b += "//\n";
  b += "// This file holds only deployment facts, no business semantics: a deploy script says what to start\n";
  b += "// (deployment and compute), a workflow says what to do (orchestration). The latter must be written by a human\n";
  b += "// -- the importer does not invent it (D-023 path A), so below is only a placeholder workflow.\n";
  b += "//\n";
  b += "// Steps in the plan (in order, for human reference only):\n";
  for (size_t i = 0; i < steps.size(); ++i) {
    const std::string note = steps[i].has_note ? one_line(steps[i].note) : std::string();
    b += "//   " + std::to_string(i + 1) + ". " + steps[i].kind;
    if (!note.empty()) b += " — " + note;
    b += "\n";
  }
  if (!models.empty()) {
    b += "//\n";
    b += "// Models seen at import time. At runtime /v1/models is authoritative; this is only the intent then:\n";
    for (const std::string& m : models) {
      const size_t eq = m.find('=');
      if (eq != std::string::npos) {
        b += "//   " + one_line(m.substr(0, eq)) + " = " + one_line(m.substr(eq + 1)) + "\n";
      } else {
        b += "//   " + one_line(m) + "\n";
      }
    }
    if (!default_model.empty()) b += "//   default model (at import): " + one_line(default_model) + "\n";
  }
  b += "\n";
  if (!http_addr.empty()) {
    b += "// The gateway address comes from `--http " + one_line(http_raw) +
         "` in the plan. A hard-coded literal is the import semantics,\n";
    b += "// not a recommendation: distributions should use env(...) so the runtime environment decides the address.\n";
    b += "backend homonoia = \"" + escape_lit(http_addr) + "\";\n\n";
  }
  b += "workflow imported";
  b += http_addr.empty() ? "()" : "(backend = homonoia)";
  b += " entry -> string {\n";
  b += "    // Placeholder body: lets the frontend accept the file (zero entries are rejected by E3021), no business meaning.\n";
  b += "    // When writing real orchestration, use model.generate / tool.call; remember to #link the matching std module.\n";
  b += "    return \"TODO: describe what this workflow should do\";\n";
  b += "}\n";

  // warnings JSON
  hm::JVal w = hm::JVal::array();
  for (const std::string& m : warnings) w.push(hm::JVal::string(m));
  const std::string wj = w.str();

  const size_t need_src = b.size() + 1;
  const size_t need_warn = wj.size() + 1;
  if (need_src > cap || (out_warn != nullptr && need_warn > warn_cap)) {
    *out_len = need_src;
    if (warn_len != nullptr) *warn_len = need_warn;
    return -4;
  }
  std::memcpy(out_src, b.data(), b.size());
  out_src[b.size()] = 0;
  *out_len = need_src;
  if (out_warn != nullptr) {
    std::memcpy(out_warn, wj.data(), wj.size());
    out_warn[wj.size()] = 0;
  }
  if (warn_len != nullptr) *warn_len = need_warn;
  return 0;
}
