// schema_validate.cpp —— JSON Schema 的**校验**（生成早已在 ir_assemble.cpp）。
//
// 为什么自己写而不是引库：零第三方是红线，而我们只需要自己生成的那一小撮
// 关键字。遇到不认识的**明确报错/明确不覆盖**，不跳过 —— 跳过等于放行。
//
// 与 Go 版（internal/schema）的分工：Go 版仍是运行期（engine 校验模型响应）的
// 实现；这一份给**消费方**（CLI / 租户校验）用。两侧由 golden 判据对齐，
// 覆盖不到的关键字（pattern / minimum / maximum）返回"未覆盖"，由调用方回落。
#include "hermes_parser.h"
#include "json_write.h"

#include <algorithm>
#include <cstring>
#include <string>
#include <vector>

namespace hm {
namespace {

// path_or_root：JSON Pointer 形态；根路径用 "/"。
std::string path_or_root(const std::string& p) { return p.empty() ? "/" : p; }

const char* type_name(const JVal* v) {
  if (v == nullptr) return "null";
  switch (v->kind()) {
    case JVal::kNull: return "null";
    case JVal::kBool: return "boolean";
    case JVal::kInt: return "number";
    case JVal::kStr: return "string";
    case JVal::kArr: return "array";
    case JVal::kObj: return "object";
    default: return "unknown";
  }
}

bool is_number(const JVal* v) { return v != nullptr && v->kind() == JVal::kInt; }

// same_json：数字按数值比，其余按 canonical 字节比（与 Go 版语义一致）。
bool same_json(const JVal* a, const JVal* b) {
  if (a == nullptr || b == nullptr) return a == b;
  if (is_number(a) && is_number(b)) return a->as_int(0) == b->as_int(0);
  return a->str() == b->str();
}

struct Ctx {
  const JVal* defs = nullptr;
  std::string err;  // 失败时 "path: reason"
  bool covered = true;  // false = 遇到未覆盖关键字
};

bool visit(const JVal* s, const JVal* v, const std::string& path, Ctx* c);

// resolve_ref 只支持 `#/$defs/Name`（生成的 schema 是自包含的）。
const JVal* resolve_ref(const std::string& ref, const JVal* defs, std::string* why) {
  const std::string prefix = "#/$defs/";
  if (ref.compare(0, prefix.size(), prefix) != 0) {
    *why = "only #/$defs/... $refs are supported, got \"" + ref + "\"";
    return nullptr;
  }
  const std::string name = ref.substr(prefix.size());
  const JVal* d = defs ? defs->find(name) : nullptr;
  if (d == nullptr) {
    *why = "$ref points to a nonexistent definition \"" + name + "\"";
    return nullptr;
  }
  return d;
}

bool fail(Ctx* c, const std::string& path, const std::string& reason) {
  c->err = path_or_root(path) + ": " + reason;
  return false;
}

bool check_type(const std::string& t, const JVal* v, const std::string& path, Ctx* c) {
  bool ok = false;
  if (t == "object") ok = v != nullptr && v->kind() == JVal::kObj;
  else if (t == "array") ok = v != nullptr && v->kind() == JVal::kArr;
  else if (t == "string") ok = v != nullptr && v->kind() == JVal::kStr;
  else if (t == "boolean") ok = v != nullptr && v->kind() == JVal::kBool;
  else if (t == "integer") ok = is_number(v);
  else return fail(c, path, "validator does not support type " + t);
  if (!ok) {
    return fail(c, path, std::string("expected ") + t + ", got " + type_name(v));
  }
  return true;
}

bool visit(const JVal* s, const JVal* v, const std::string& path, Ctx* c) {
  if (s == nullptr || s->kind() != JVal::kObj) return fail(c, path, "schema is not an object");

  // $ref 优先：有它时其余关键字按 JSON Schema 语义应被忽略。
  const JVal* ref = s->find("$ref");
  if (ref != nullptr && ref->kind() == JVal::kStr) {
    std::string why;
    const JVal* target = resolve_ref(ref->as_str(), c->defs, &why);
    if (target == nullptr) return fail(c, path, why);
    return visit(target, v, path, c);
  }

  const JVal* tv = s->find("type");
  if (tv == nullptr || tv->kind() != JVal::kStr) {
    return fail(c, path, "schema is missing type (the validator refuses to guess)");
  }
  const std::string t = tv->as_str();
  if (!check_type(t, v, path, c)) return false;

  // 键按字节序遍历：同输入同报错，便于与 golden 比对。
  std::vector<std::string> keys;
  s->keys(&keys);
  std::sort(keys.begin(), keys.end());
  for (const std::string& k : keys) {
    const JVal* kv = s->find(k);
    if (k == "enum") {
      if (kv == nullptr || kv->kind() != JVal::kArr) return fail(c, path, "enum is not an array");
      if (kv->size() == 0) {
        return fail(c, path,
                    "enum cannot be empty (an empty enum rejects everything and is almost always a mistake)");
      }
      bool hit = false;
      for (size_t i = 0; i < kv->size(); ++i) {
        if (same_json(kv->at(i), v)) { hit = true; break; }
      }
      if (!hit) {
        return fail(c, path, "not in enum: " + (v ? v->str() : "null"));
      }
    } else if (k == "pattern" || k == "minimum" || k == "maximum") {
      // 未覆盖：Go 版用 RE2 / 整数比较。这里如实交回，由调用方回落 Go 实现 ——
      // 半截实现（比如用 std::regex 冒充 RE2）会造成"两边对同一个 schema
      // 给出不同结论"，那比不覆盖坏得多。
      c->covered = false;
      return false;
    } else if (k == "type" || k == "$defs" || k == "$schema" || k == "title" || k == "description") {
      continue;
    } else if (k == "properties") {
      if (kv == nullptr || kv->kind() != JVal::kObj) return fail(c, path, "properties is not an object");
      if (v == nullptr || v->kind() != JVal::kObj) {
        return fail(c, path, std::string("expected object, got ") + type_name(v));
      }
      std::vector<std::string> names;
      kv->keys(&names);
      std::sort(names.begin(), names.end());
      for (const std::string& name : names) {
        const JVal* sub = kv->find(name);
        if (sub == nullptr || sub->kind() != JVal::kObj) {
          return fail(c, path, "field " + name + " has a non-object schema");
        }
        const JVal* fv = v->find(name);
        if (fv != nullptr) {
          if (!visit(sub, fv, path + "/" + name, c)) return false;
        }
      }
    } else if (k == "required") {
      if (kv == nullptr || kv->kind() != JVal::kArr) return fail(c, path, "required is not an array");
      if (v == nullptr || v->kind() != JVal::kObj) {
        return fail(c, path, std::string("expected object, got ") + type_name(v));
      }
      for (size_t i = 0; i < kv->size(); ++i) {
        const JVal* r = kv->at(i);
        if (r == nullptr || r->kind() != JVal::kStr) continue;
        if (v->find(r->as_str()) == nullptr) {
          return fail(c, path, "missing required field " + r->as_str());
        }
      }
    } else if (k == "additionalProperties") {
      if (kv == nullptr || kv->kind() != JVal::kBool) {
        return fail(c, path, "additionalProperties only supports booleans");
      }
      if (kv->as_bool(false)) continue;
      if (v == nullptr || v->kind() != JVal::kObj) {
        return fail(c, path, std::string("expected object, got ") + type_name(v));
      }
      const JVal* props = s->find("properties");
      std::vector<std::string> extra;
      std::vector<std::string> names;
      v->keys(&names);
      for (const std::string& name : names) {
        if (props == nullptr || props->find(name) == nullptr) extra.push_back(name);
      }
      if (!extra.empty()) {
        std::sort(extra.begin(), extra.end());
        std::string joined;
        for (size_t i = 0; i < extra.size(); ++i) {
          if (i) joined += ", ";
          joined += extra[i];
        }
        return fail(c, path, "undeclared fields: " + joined);
      }
    } else if (k == "items") {
      if (kv == nullptr || kv->kind() != JVal::kObj) return fail(c, path, "items is not an object");
      if (v == nullptr || v->kind() != JVal::kArr) {
        return fail(c, path, std::string("expected array, got ") + type_name(v));
      }
      for (size_t i = 0; i < v->size(); ++i) {
        if (!visit(kv, v->at(i), path + "/" + std::to_string(i), c)) return false;
      }
    } else if (k == "$ref") {
      continue;  // 已在入口处理
    } else {
      return fail(c, path,
                  "validator does not recognize keyword " + k +
                      " (refusing to skip: skipping means allowing)");
    }
  }
  return true;
}

}  // namespace
}  // namespace hm

#pragma GCC visibility push(default)
extern "C" int hm_schema_validate(const char* schema_json, size_t schema_len,
                                  const char* value_json, size_t value_len,
                                  char* out_err, size_t err_cap, size_t* err_len) {
  using namespace hm;
  size_t pos = 0;
  JVal schema;
  if (schema_json == nullptr || !parse_json(schema_json, schema_len, &schema, &pos)) {
    const std::string m = "schema is not valid JSON";
    std::memcpy(out_err, m.data(), m.size() < err_cap ? m.size() : err_cap);
    if (err_len) *err_len = m.size();
    return 3;
  }
  pos = 0;
  JVal value;
  if (value_json == nullptr || !parse_json(value_json, value_len, &value, &pos)) {
    const std::string m = "/: value is not valid JSON";
    std::memcpy(out_err, m.data(), m.size() < err_cap ? m.size() : err_cap);
    if (err_len) *err_len = m.size();
    return 3;
  }
  Ctx c;
  const JVal* defs = schema.find("$defs");
  c.defs = defs;
  if (visit(&schema, &value, "", &c)) {
    if (err_len) *err_len = 0;
    return 0;
  }
  if (!c.covered) {
    const std::string m = "schema keyword is not covered by the C++ validator";
    std::memcpy(out_err, m.data(), m.size() < err_cap ? m.size() : err_cap);
    if (err_len) *err_len = m.size();
    return 1;
  }
  std::memcpy(out_err, c.err.data(), c.err.size() < err_cap ? c.err.size() : err_cap);
  if (err_len) *err_len = c.err.size();
  return 2;
}
#pragma GCC visibility pop
