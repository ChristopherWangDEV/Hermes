// ir_assemble.cpp —— 源码 → **最终规范 IR JSON**。`internal/ir` C++ 化的目标位置。
//
// 今天这条链路是：
//   源码 → C++ 前端（workflow JSON）→ Go 装配（internal/ir）→ Go 结构体 → 再序列化
//
// 目标：装配整段搬进 C++，前端直接吐出最终 IR。跨语言只剩一次读取。
//
// 分工不变（LANGUAGE §1.2：C++ 前端**无 IO**）：读文件、算 sha256、include 解析由
// 调用方负责，模块表经 modules_json 传进来。装配本身（节点重排、上界、canonical
// 写出）在这里做。
//
// 迁移策略与 C 后端一致：未覆盖就返回 code 1，调用方回落 Go 版；不编半截产物。
#include "hermes_parser.h"
#include "json_write.h"

// Rust 侧导出的纯 SHA-256（见 native/rust/src/lib.rs 的 hm_sha256）。
// 复用已审计的实现，而不是在 C++ 里再写一份加密原语。
extern "C" int hm_sha256(const unsigned char* data, size_t len, unsigned char* out);

#include <cstdint>
#include <cstring>
#include <map>
#include <string>
#include <vector>

namespace hm {
namespace {

bool put(const std::string& text, char* out, size_t cap, size_t* out_len) {
  if (out_len) *out_len = text.size();
  if (out == nullptr || cap < text.size()) return false;
  if (!text.empty()) std::memcpy(out, text.data(), text.size());
  return true;
}

// kOmitEmpty 是从 internal/ir/types.go 的 `json:"…,omitempty"` 抽出来的键集合。
// **只有**这些键在空值时被丢掉；其余键（如 input / interp_deps）在 Go 里没有
// omitempty，即使值是 null / [] 也必须写出来 —— 一刀切会把它们删掉，
// 而那种差异会让下层读到的形状与今天不同。
const char* const kOmitEmpty[] = {
    "agent",
    "archive",
    "arg_types",
    "args",
    "arms",
    "auth",
    "backend",
    "bind",
    "body",
    "branches",
    "c",
    "captures",
    "catches",
    "chan",
    "cond",
    "const",
    "data",
    "decl_loc",
    "def_loc",
    "default",
    "delay_ms",
    "delta",
    "description",
    "e",
    "elifs",
    "else",
    "env",
    "exported",
    "fn",
    "functions",
    "header",
    "i",
    "id",
    "implicit",
    "init",
    "join",
    "join_k",
    "key",
    "kids",
    "l",
    "lit",
    "loc",
    "main",
    "max_par",
    "max_parallel_tools",
    "max_steps",
    "member_sha256",
    "model",
    "ms",
    "n",
    "name",
    "num",
    "of",
    "op",
    "opt",
    "params",
    "parts",
    "path",
    "r",
    "ref",
    "ret",
    "route",
    "schema",
    "security",
    "sha256",
    "signal",
    "source_sha256",
    "subj",
    "t",
    "target",
    "then",
    "timeout_ms",
    "to",
    "tool",
    "unbounded",
    "v",
    "value",
    "var",
    "vars",
};

bool is_omitempty_key(const std::string& k) {
  for (const char* e : kOmitEmpty) {
    if (k == e) return true;
  }
  return false;
}

// keep_empty_for 是"即使为空也必须写出来"的键，按节点 kind 分组。
// 这张表是扫 41 份基线得到的**事实**，不是从 tag 推断的结论 ——
// 产物有自定义编码路径，按 tag 推会得到"看起来合理但字节不同"的形状。
bool keep_empty_for(const std::string& kind, const std::string& key) {
  if (kind == "call_model") return key == "attempt_ctx" || key == "schema";
  if (kind == "call_tool") return key == "attempt_ctx";
  if (kind == "emit") return key == "args";
  return false;
}

// prune_empty 递归丢掉对象里的 null 与空数组（Go 的 omitempty 语义）。
// 空字符串与 0 刻意保留：它们对应 Go 侧的显式值或指针，omitempty 不会丢。
void prune_empty(JVal& v) {
  if (v.kind() == JVal::kArr) {
    for (size_t i = 0; i < v.size(); ++i) {
      JVal* e = const_cast<JVal*>(v.at(i));
      if (e != nullptr) prune_empty(*e);
    }
    return;
  }
  if (v.kind() != JVal::kObj) return;
  // JVal 没有"删除键"接口，所以重建一份：先取全部键名，再按规则筛选。
  JVal rebuilt = JVal::object();
  std::vector<std::string> keys;
  v.keys(&keys);
  for (const std::string& k : keys) {
    const JVal* child = v.find(k);
    if (child == nullptr) continue;
    // 有些键即使为空也必须写出来 —— 按节点 kind 分组，见 keep_empty_for。
    // 这条不能按 tag 推：产物不经普通 struct marshal，有自定义编码路径。
    const std::string kind = [&]() {
      const JVal* kv = v.find("k");
      return (kv != nullptr && kv->kind() == JVal::kStr) ? kv->as_str() : std::string();
    }();
    if (is_omitempty_key(k) && !keep_empty_for(kind, k)) {
      if (child->kind() == JVal::kNull) continue;
      if (child->kind() == JVal::kArr && child->size() == 0) continue;
    }
    JVal copy = *child;
    // 不递归进 `args`：基线里 args 内部的 null（body/header/idem_key）全部保留，
    // 递归会按 omitempty 把它们剪掉。
    if (k != "args") prune_empty(copy);
    rebuilt.set(k, std::move(copy));
  }
  v = std::move(rebuilt);
}

// ---- struct → JSON Schema -------------------------------------------------
// 对应 internal/ir/schema.go。schema 是**运行时契约**：serve 用它校验请求、
// SDK 用它生成类型。字节必须与 Go 版一致，否则同一份源码的 schema 哈希会变。
//
// 由类型 id 查名字（$ref 用）。
const char* type_name_of(const JVal* types, int64_t id) {
  if (types == nullptr) return nullptr;
  for (size_t i = 0; i < types->size(); ++i) {
    const JVal* t = types->at(i);
    const JVal* tid = t ? t->find("id") : nullptr;
    const JVal* nm = t ? t->find("name") : nullptr;
    if (tid != nullptr && tid->kind() == JVal::kInt && tid->as_int(-1) == id && nm != nullptr &&
        nm->kind() == JVal::kStr) {
      return nm->as_str().c_str();
    }
  }
  return nullptr;
}

// field_schema 是 IR 类型表达式 → JSON Schema（LANGUAGE §4.6）。
// 返回 false 表示这种类型不能出现在字段位置（如 tuple）。
bool field_schema(const JVal* t, const JVal* types, JVal* out, std::string* err) {
  if (t == nullptr || t->kind() != JVal::kObj) { *err = "field type is not an object"; return false; }
  const JVal* k = t->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) { *err = "field type has no k"; return false; }
  const std::string kind = k->as_str();

  if (kind == "string") { *out = JVal::object(); out->set("type", JVal::string("string")); return true; }
  if (kind == "int") { *out = JVal::object(); out->set("type", JVal::string("integer")); return true; }
  if (kind == "cost") {
    *out = JVal::object();
    out->set("type", JVal::string("integer"));
    // 单位写进 description：不写的话上游会以为这是美元。
    out->set("description", JVal::string("micro-USD"));
    return true;
  }
  if (kind == "bool") { *out = JVal::object(); out->set("type", JVal::string("boolean")); return true; }
  if (kind == "double") { *out = JVal::object(); out->set("type", JVal::string("number")); return true; }
  if (kind == "json") {
    JVal arr = JVal::array();
    for (const char* ty : {"object", "array", "string", "integer", "number", "boolean", "null"}) {
      arr.push(JVal::string(ty));
    }
    *out = JVal::object();
    out->set("type", std::move(arr));
    return true;
  }
  if (kind == "struct") {
    const JVal* id = t->find("id");
    if (id == nullptr || id->kind() != JVal::kInt) { *err = "struct field is missing id"; return false; }
    const char* nm = type_name_of(types, id->as_int(-1));
    if (nm == nullptr) { *err = "struct field references nonexistent type id"; return false; }
    *out = JVal::object();
    out->set("$ref", JVal::string(std::string("#/$defs/") + nm));
    return true;
  }
  if (kind == "array") {
    const JVal* of = t->find("of");
    if (of == nullptr) { *err = "array is missing its element type"; return false; }
    JVal elem;
    if (!field_schema(of, types, &elem, err)) return false;
    *out = JVal::object();
    out->set("type", JVal::string("array"));
    out->set("items", std::move(elem));
    return true;
  }
  if (kind == "httpresult") {
    // HttpResult<T> = { int status; T body; json headers; }（§5.6）。
    const JVal* of = t->find("of");
    if (of == nullptr) { *err = "httpresult is missing its body type"; return false; }
    JVal body;
    if (!field_schema(of, types, &body, err)) return false;
    JVal props = JVal::object();
    JVal st = JVal::object();
    st.set("type", JVal::string("integer"));
    props.set("status", std::move(st));
    props.set("body", std::move(body));
    JVal h = JVal::object();
    h.set("type", JVal::string("object"));
    props.set("headers", std::move(h));
    JVal req = JVal::array();
    req.push(JVal::string("status"));
    req.push(JVal::string("body"));
    req.push(JVal::string("headers"));
    *out = JVal::object();
    out->set("type", JVal::string("object"));
    out->set("properties", std::move(props));
    out->set("required", std::move(req));
    out->set("additionalProperties", JVal::boolean(false));
    return true;
  }
  if (kind == "tuple") {
    *err = "tuple cannot be stored or named (IR.md §3.1)";
    return false;
  }
  *err = "unknown type " + kind;
  return false;
}

// object_schema 生成一个 struct 的 schema；with_defs 为真时附带 $defs。
bool object_schema(const JVal* t, const JVal* types, bool with_defs, JVal* out, std::string* err) {
  const JVal* fields = t ? t->find("fields") : nullptr;
  JVal props = JVal::object();
  JVal required = JVal::array();
  if (fields != nullptr) {
    for (size_t i = 0; i < fields->size(); ++i) {
      const JVal* f = fields->at(i);
      const JVal* nm = f ? f->find("n") : nullptr;
      const JVal* ty = f ? f->find("t") : nullptr;
      const JVal* opt = f ? f->find("opt") : nullptr;
      if (nm == nullptr || nm->kind() != JVal::kStr || ty == nullptr) {
        *err = "struct field is malformed";
        return false;
      }
      JVal inner;
      if (!field_schema(ty, types, &inner, err)) return false;
      // 可选字段也进 required：类型写成 anyOf[T, null]，于是"字段缺失"与
      // "字段为 null"在协议层是同一件事（LANGUAGE §8.3 规则 6）。
      if (opt != nullptr && opt->kind() == JVal::kBool && opt->as_bool(false)) {
        JVal any = JVal::array();
        any.push(std::move(inner));
        JVal null_ty = JVal::object();
        null_ty.set("type", JVal::string("null"));
        any.push(std::move(null_ty));
        JVal wrapper = JVal::object();
        wrapper.set("anyOf", std::move(any));
        props.set(nm->as_str(), std::move(wrapper));
      } else {
        props.set(nm->as_str(), std::move(inner));
      }
      required.push(JVal::string(nm->as_str()));
    }
  }
  *out = JVal::object();
  out->set("type", JVal::string("object"));
  out->set("properties", std::move(props));
  out->set("required", std::move(required));
  out->set("additionalProperties", JVal::boolean(false));

  if (with_defs && types != nullptr && types->size() > 1) {
    const char* self = nullptr;
    {
      const JVal* nm = t->find("name");
      if (nm != nullptr && nm->kind() == JVal::kStr) self = nm->as_str().c_str();
    }
    JVal defs = JVal::object();
    for (size_t i = 0; i < types->size(); ++i) {
      const JVal* other = types->at(i);
      const JVal* onm = other ? other->find("name") : nullptr;
      if (onm == nullptr || onm->kind() != JVal::kStr) continue;
      if (self != nullptr && onm->as_str() == self) continue;
      // $defs 内部的类型不再各自带 $defs（扁平表）：嵌套会让同一类型在不同
      // 根 schema 里出现不同字节，V-8 的比对就失去意义。
      JVal shallow;
      if (!object_schema(other, types, false, &shallow, err)) return false;
      defs.set(onm->as_str(), std::move(shallow));
    }
    out->set("$defs", std::move(defs));
  }
  return true;
}

// drop_key 重建一个不含指定键的对象（JVal 没有删除接口）。
void drop_key(JVal* v, const char* key) {
  if (v == nullptr || v->kind() != JVal::kObj) return;
  std::vector<std::string> names;
  v->keys(&names);
  JVal rebuilt = JVal::object();
  for (const std::string& k : names) {
    if (k == key) continue;
    const JVal* child = v->find(k);
    if (child != nullptr) rebuilt.set(k, *child);
  }
  *v = std::move(rebuilt);
}

void resolve_type_refs(JVal* t, const JVal* types) {
  if (t == nullptr || t->kind() != JVal::kObj) return;
  const JVal* k = t->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) return;
  if (k->as_str() == "struct") {
    const JVal* ref = t->find("ref");
    const JVal* id = t->find("id");
    if (id == nullptr && ref != nullptr && ref->kind() == JVal::kStr && types != nullptr) {
      for (size_t i = 0; i < types->size(); ++i) {
        const JVal* ty = types->at(i);
        const JVal* nm = ty ? ty->find("name") : nullptr;
        const JVal* tid = ty ? ty->find("id") : nullptr;
        if (nm != nullptr && nm->kind() == JVal::kStr && nm->as_str() == ref->as_str() &&
            tid != nullptr && tid->kind() == JVal::kInt) {
          t->set("id", JVal::integer(tid->as_int(-1)));
          // ref 必须在解成 id 之后**删掉**：Go 侧解析完把 Ref 置空，omitempty
          // 于是不写出；留着它产物里会多一个重复事实（名字 + 下标），
          // 下游按 id 读、按 ref 比会得到两套答案。
          drop_key(t, "ref");
          break;
        }
      }
    }
    return;
  }
  const JVal* of = t->find("of");
  if (of != nullptr) resolve_type_refs(const_cast<JVal*>(of), types);
}


// normalize_type_expr：把"名字塞在 k 里"的类型表达式规范成 IR 的形状。
//
// 前端给节点 ret/to 时用的是 `{"k":"Query"}`（把类型名直接当 k），因为那是
// AST 的 type_of 形态；IR 要求 `{"k":"struct","id":N}`。不规范化，下游按 k
// 查基础类型表会查不到，表现成"未知类型 Query"。
void normalize_type_expr(JVal* t, const JVal* types) {
  if (t == nullptr || t->kind() != JVal::kObj) return;
  const JVal* of = t->find("of");
  if (of != nullptr) normalize_type_expr(const_cast<JVal*>(of), types);
  const JVal* k = t->find("k");
  if (k == nullptr || k->kind() != JVal::kStr) return;
  const std::string name = k->as_str();
  // `{"k":"struct","ref":"X"}`：ref 解成 id 后必须删掉。
  // （resolve_type_refs 只处理类型表里的字段，节点上的 ret/to 要在这里补。）
  // 这一步必须在下面的基础类型早退**之前** —— "struct" 是基础类型名，
  // 早退之后这段就永远跑不到，ref 会原样留在产物里。
  if (name == "struct") {
    resolve_type_refs(t, types);
    return;
  }
  static const char* kBase[] = {"string", "int", "bool",   "double", "json",
                                "cost",   "char", "short", "long",  "struct",
                                "array",  "httpresult", "tuple"};
  for (const char* b : kBase) {
    if (name == b) return;
  }
  // 不是基础类型名 → 它是本 unit 的 struct 名字，换成 id 形态。
  const char* ignored = type_name_of(types, -1);
  (void)ignored;
  if (types == nullptr) return;
  for (size_t i = 0; i < types->size(); ++i) {
    const JVal* ty = types->at(i);
    const JVal* nm = ty ? ty->find("name") : nullptr;
    const JVal* tid = ty ? ty->find("id") : nullptr;
    if (nm != nullptr && nm->kind() == JVal::kStr && nm->as_str() == name && tid != nullptr &&
        tid->kind() == JVal::kInt) {
      t->set("k", JVal::string("struct"));
      t->set("id", JVal::integer(tid->as_int(-1)));
      return;
    }
  }
}

// ---- 类型表：分配 id + 算内容摘要 ------------------------------------------
// 对应 internal/ir/assemble.go 的 resolveTypes / typeSHA256。
//
// id 按**声明顺序**从 1 起（前向引用合法，所以先建完整张表再解析引用）。
// 摘要只覆盖 {"name","fields"}，**不含 id/module/loc**：链接期比较两个库里的
// 同名 struct 时，它们在各自源码里的行号必然不同，带上 loc 会让每个 struct
// 都"不一致"，ODR 直接失效。
std::string hex32(const unsigned char* d) {
  static const char* kHex = "0123456789abcdef";
  std::string out;
  out.reserve(64);
  for (int i = 0; i < 32; ++i) {
    out.push_back(kHex[d[i] >> 4]);
    out.push_back(kHex[d[i] & 0x0f]);
  }
  return out;
}

bool type_sha256(const JVal* t, std::string* out_hex, std::string* err) {
  const JVal* nm = t ? t->find("name") : nullptr;
  const JVal* fields = t ? t->find("fields") : nullptr;
  JVal field_vals = JVal::array();
  if (fields != nullptr) {
    for (size_t i = 0; i < fields->size(); ++i) {
      const JVal* f = fields->at(i);
      const JVal* fn = f ? f->find("n") : nullptr;
      const JVal* ft = f ? f->find("t") : nullptr;
      const JVal* fopt = f ? f->find("opt") : nullptr;
      const JVal* fann = f ? f->find("ann") : nullptr;
      if (fn == nullptr || ft == nullptr) { *err = "malformed field in type table"; return false; }
      // t 只保留 k 与（有则）id：其余键不进摘要。
      JVal tv = JVal::object();
      const JVal* k = ft->find("k");
      if (k != nullptr) tv.set("k", *k);
      const JVal* id = ft->find("id");
      if (id != nullptr) tv.set("id", *id);
      JVal anns = JVal::array();
      if (fann != nullptr && fann->kind() == JVal::kArr) {
        for (size_t a = 0; a < fann->size(); ++a) {
          const JVal* ann = fann->at(a);
          JVal av = JVal::object();
          const JVal* an = ann ? ann->find("name") : nullptr;
          if (an != nullptr) av.set("name", *an);
          const JVal* args = ann ? ann->find("args") : nullptr;
          av.set("args", args != nullptr ? *args : JVal::array());
          anns.push(std::move(av));
        }
      }
      JVal fv = JVal::object();
      fv.set("n", *fn);
      fv.set("t", std::move(tv));
      fv.set("opt", fopt != nullptr ? *fopt : JVal::boolean(false));
      fv.set("ann", std::move(anns));
      field_vals.push(std::move(fv));
    }
  }
  JVal root = JVal::object();
  root.set("name", nm != nullptr ? *nm : JVal::string(""));
  root.set("fields", std::move(field_vals));
  const std::string bytes = root.str();
  unsigned char digest[32];
  if (hm_sha256(reinterpret_cast<const unsigned char*>(bytes.data()), bytes.size(), digest) != 0) {
    *err = "sha256 failed";
    return false;
  }
  *out_hex = hex32(digest);
  return true;
}

// assign_type_ids 按声明顺序分配 1..n，并算每型的摘要。
bool assign_type_ids(JVal* types, std::string* err) {
  if (types == nullptr || types->kind() != JVal::kArr) return true;
  for (size_t i = 0; i < types->size(); ++i) {
    JVal* t = const_cast<JVal*>(types->at(i));
    if (t == nullptr || t->kind() != JVal::kObj) continue;
    t->set("id", JVal::integer(static_cast<int64_t>(i) + 1));
    const JVal* mod = t->find("module");
    if (mod == nullptr) t->set("module", JVal::integer(0));
  }
  // 引用可能在后面声明，所以 id 全部就位之后才解析 ref。
  for (size_t i = 0; i < types->size(); ++i) {
    JVal* t = const_cast<JVal*>(types->at(i));
    const JVal* fields = t ? t->find("fields") : nullptr;
    if (fields == nullptr) continue;
    for (size_t fi = 0; fi < fields->size(); ++fi) {
      JVal* f = const_cast<JVal*>(fields->at(fi));
      JVal* ft = f ? const_cast<JVal*>(f->find("t")) : nullptr;
      if (ft != nullptr) resolve_type_refs(ft, types);
    }
  }
  for (size_t i = 0; i < types->size(); ++i) {
    JVal* t = const_cast<JVal*>(types->at(i));
    std::string hex;
    if (!type_sha256(t, &hex, err)) return false;
    t->set("sha256", JVal::string(hex));
  }
  return true;
}

// resolve_type_refs 把类型表达式里的 `ref`（类型名）换成 `id`。
// 前端产出的是名字：它不认识"装配后才知道的 id 表"。这一步漏了，
// struct 字段会被判成"missing id"，整类带 struct 的源码都装配不出来。

// build_schemas 产出 schemas[]：每个 struct 一份 {id, json, root}。
bool build_schemas(const JVal* types, JVal* out, std::string* err) {
  *out = JVal::array();
  if (types == nullptr || types->kind() != JVal::kArr) return true;
  for (size_t i = 0; i < types->size(); ++i) {
    const JVal* t = types->at(i);
    JVal schema;
    if (!object_schema(t, types, true, &schema, err)) return false;
    const JVal* nm = t ? t->find("name") : nullptr;
    JVal doc = JVal::object();
    doc.set("id", JVal::string(nm && nm->kind() == JVal::kStr ? nm->as_str() : ""));
    doc.set("json", std::move(schema));
    doc.set("root", JVal::string(nm && nm->kind() == JVal::kStr ? nm->as_str() : ""));
    out->push(std::move(doc));
  }
  return true;
}


// base_name 只留文件名部分（与 Go 侧 filepath.Base 同义）。
std::string base_name(const char* filename) {
  std::string s(filename ? filename : "");
  const size_t slash = s.find_last_of("/\\");
  if (slash != std::string::npos) s = s.substr(slash + 1);
  return s;
}

// unit name 取入口文件名的主干：hello.hermes → hello。
std::string unit_name(const char* filename) {
  std::string s(filename ? filename : "");
  const size_t slash = s.find_last_of("/\\");
  if (slash != std::string::npos) s = s.substr(slash + 1);
  const size_t dot = s.find_last_of('.');
  if (dot != std::string::npos && dot > 0) s = s.substr(0, dot);
  return s;
}

// call_frontend 调同一个库里的前端入口，拿到 workflow 对象的 JSON 数组。
// 不重复实现任何解析逻辑 —— 前端就是权威。
int call_frontend(const uint8_t* src, size_t len, const char* filename, const std::string& ext,
                  std::string* wf_json, std::string* diag) {
  std::vector<char> buf(64 * 1024);
  std::vector<char> dbuf(16 * 1024);
  size_t wlen = 0, dlen = 0;
  for (int attempt = 0; attempt < 3; ++attempt) {
    const int rc = hm_codegen_ext(src, len, filename, 0, ext.data(), ext.size(),
                                  buf.data(), buf.size(), &wlen,
                                  dbuf.data(), dbuf.size(), &dlen);
    if (rc == 0) {
      wf_json->assign(buf.data(), wlen);
      diag->assign(dbuf.data(), dlen);
      return 0;
    }
    // 缓冲不足：按回填大小扩容重试。其它失败就是源码错误。
    if (wlen > buf.size()) { buf.resize(wlen); continue; }
    if (dlen > dbuf.size()) { dbuf.resize(dlen); continue; }
    diag->assign(dbuf.data(), dlen);
    return 1;
  }
  return 2;
}

// call_table 调前端的某个"表"入口（tools / backends），拿 JSON 数组。
// 与 call_frontend 同一套缓冲协商：不猜大小，按回填扩。
using TableEntry = int (*)(const uint8_t*, size_t, const char*, int, char*, size_t, size_t*,
                           char*, size_t, size_t*);

bool call_table(TableEntry fn, const uint8_t* src, size_t len, const char* filename,
                JVal* out, const char* what) {
  std::vector<char> buf(16 * 1024);
  std::vector<char> dbuf(4 * 1024);
  size_t wlen = 0, dlen = 0;
  for (int attempt = 0; attempt < 3; ++attempt) {
    const int rc = fn(src, len, filename, 0, buf.data(), buf.size(), &wlen, dbuf.data(),
                      dbuf.size(), &dlen);
    if (rc == 0) {
      size_t pos = 0;
      if (wlen == 0) { *out = JVal::array(); return true; }
      return parse_json(buf.data(), wlen, out, &pos);
    }
    if (wlen > buf.size()) { buf.resize(wlen); continue; }
    if (dlen > dbuf.size()) { dbuf.resize(dlen); continue; }
    (void)what;
    return false;
  }
  return false;
}

// call_functions 调前端的函数表入口（多一个 ext_types 参数）。
bool call_functions(const uint8_t* src, size_t len, const char* filename, const std::string& ext,
                    JVal* out) {
  std::vector<char> buf(32 * 1024);
  std::vector<char> dbuf(8 * 1024);
  size_t wlen = 0, dlen = 0;
  for (int attempt = 0; attempt < 3; ++attempt) {
    const int rc = hm_codegen_functions(src, len, filename, 0, ext.data(), ext.size(),
                                        buf.data(), buf.size(), &wlen, dbuf.data(), dbuf.size(),
                                        &dlen);
    if (rc == 0) {
      if (wlen == 0) { *out = JVal::array(); return true; }
      size_t pos = 0;
      return parse_json(buf.data(), wlen, out, &pos);
    }
    if (wlen > buf.size()) { buf.resize(wlen); continue; }
    if (dlen > dbuf.size()) { dbuf.resize(dlen); continue; }
    return false;
  }
  return false;
}

// call_table_h 与 call_table 相同，但显式指定 is_header：多模块装配里
// .h 与 .hermes 要走不同的前端路径（.h 没有函数体）。
bool call_table_h(TableEntry fn, const uint8_t* src, size_t len, const char* filename,
                  bool is_header, JVal* out) {
  std::vector<char> buf(32 * 1024);
  std::vector<char> dbuf(4 * 1024);
  size_t wlen = 0, dlen = 0;
  for (int attempt = 0; attempt < 3; ++attempt) {
    const int rc = fn(src, len, filename, is_header ? 1 : 0, buf.data(), buf.size(), &wlen,
                      dbuf.data(), dbuf.size(), &dlen);
    if (rc == 0) {
      if (wlen == 0) { *out = JVal::array(); return true; }
      size_t pos = 0;
      return parse_json(buf.data(), wlen, out, &pos);
    }
    if (wlen > buf.size()) { buf.resize(wlen); continue; }
    if (dlen > dbuf.size()) { dbuf.resize(dlen); continue; }
    return false;
  }
  return false;
}

// call_types 调前端拿类型表（struct 定义）。同一份源码，权威仍在 C++ 前端。
int call_types(const uint8_t* src, size_t len, const char* filename, std::string* out_json) {
  std::vector<char> buf(32 * 1024);
  std::vector<char> dbuf(8 * 1024);
  size_t wlen = 0, dlen = 0;
  for (int attempt = 0; attempt < 3; ++attempt) {
    const int rc = hm_codegen_types(src, len, filename, 0, buf.data(), buf.size(), &wlen,
                                    dbuf.data(), dbuf.size(), &dlen);
    if (rc == 0) { out_json->assign(buf.data(), wlen); return 0; }
    if (wlen > buf.size()) { buf.resize(wlen); continue; }
    if (dlen > dbuf.size()) { dbuf.resize(dlen); continue; }
    return 1;
  }
  return 2;
}

}  // namespace
}  // namespace hm

// =====================================================================
// 静态上界独立复算（镜像 internal/ir/bounds.go）
//
// 这是装配层的**第二份实现**：node 图定型之后，按 IR.md §5.1 的代数从根
// 重算 max_spans / max_model_calls / max_tool_calls。Go 侧的 bounds.go 是
// 第一份，两份用不同语言算出同一个数，才算真的钉住了上界。
//
// 与 codegen.cpp 的关系：codegen 在**前端**把一份 bounds 写进 workflow；这里
// 在**装配层**重新复算代数三字段（max_spans / max_model_calls / max_tool_calls）
// 并覆盖回去。codegen 的 max_steps（= stmtNodes_+1）、max_concurrency、
// max_retry_expansion、unbounded 是装配期不掌握的计数器，沿用前端值。
//
// 哨兵语义与 Go 完全一致：
//   -2 (kBoundUnrec)  编译器 bug：遇到不认识的节点种类，必须响亮报出；
//   -1 (kBoundUnk)    静态上界合法地算不出来（while / 无界 agent / 未知循环），
//                      诚实地说"不知道"，不编一个数字。
// 约束：本文件随 libhermes_parser.a 编译，带 -fno-exceptions -fno-rtti，
// 所以这里不依赖异常 / RTTI / std::function。
// =====================================================================
namespace hm {
namespace {

const int kBoundUnrec = -2;
const int kBoundUnk = -1;
const int64_t kMaxBoundInt = INT64_MAX;

int absorb_sum(int acc, int s) {
  if (acc == kBoundUnrec || s == kBoundUnrec) return kBoundUnrec;
  if (acc == kBoundUnk || s == kBoundUnk) return kBoundUnk;
  return acc + s;
}
int absorb_max(int acc, int s) {
  if (acc == kBoundUnrec || s == kBoundUnrec) return kBoundUnrec;
  if (acc == kBoundUnk || s == kBoundUnk) return kBoundUnk;
  if (s > acc) return s;
  return acc;
}
// 用乘法折算子树上界（for 的 N 轮、retry 的 N+1 次）。哨兵原样传出：
// -1 乘任何数还是"不知道"，而不是某个数字。溢出返回 kBoundUnk（不回绕）。
int scaled_bound(int s, int64_t factor) {
  if (s < 0) return s;
  if (factor < 0) return kBoundUnk;
  int64_t prod = (int64_t)s * factor;
  if (prod > kMaxBoundInt) return kBoundUnk;
  return (int)prod;
}

// 不抛异常的十进制解析（IR 里的整数字面量可能是 JSON 整数或整数串两种形态）。
bool parse_int64(const std::string& s, int64_t* out) {
  if (s.empty()) return false;
  size_t i = 0;
  while (i < s.size() && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r')) ++i;
  bool neg = false;
  if (i < s.size() && s[i] == '+') ++i;
  else if (i < s.size() && s[i] == '-') { neg = true; ++i; }
  if (i >= s.size()) return false;
  int64_t val = 0;
  size_t start = i;
  while (i < s.size()) {
    char c = s[i];
    if (c < '0' || c > '9') break;
    int64_t d = c - '0';
    if (val > (kMaxBoundInt - d) / 10) return false;  // 防溢出
    val = val * 10 + d;
    ++i;
  }
  if (i == start) return false;
  while (i < s.size() && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r')) ++i;
  if (i != s.size()) return false;
  *out = neg ? -val : val;
  return true;
}

// 读一个 lit 节点里的整数字面量；形状必须与 codegen 一致（t=="int" 且 v 可解析）。
bool lit_int_value(const JVal* n, int64_t* out) {
  if (n == nullptr || n->kind() != JVal::kObj) return false;
  const JVal* k = n->find("k");
  const JVal* t = n->find("t");
  const JVal* v = n->find("v");
  if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "lit") return false;
  if (t == nullptr || t->kind() != JVal::kStr || t->as_str() != "int") return false;
  if (v == nullptr) return false;
  if (v->kind() == JVal::kInt) { *out = v->as_int(0); return true; }
  if (v->kind() == JVal::kStr) { int64_t x; if (parse_int64(v->as_str(), &x)) { *out = x; return true; } }
  return false;
}

// for 的静态迭代次数：逐条镜像 codegen.cpp 的 for_iterations（行 299-319）与
// internal/ir/bounds.go 的 forTripCount。init / cond / r 都是"指向节点表的 id"，
// 这里用 pos_by_id 把 id 解成节点指针（nodes[pos]）。
int for_trip_count(const std::map<int,int>& pos_by_id, const std::vector<const JVal*>& nodes, const JVal* n) {
  if (n == nullptr) return kBoundUnk;
  const JVal* initf = n->find("init");
  const JVal* condf = n->find("cond");
  if (initf == nullptr || condf == nullptr) return kBoundUnk;
  int64_t delta = 1;
  const JVal* df = n->find("delta");
  if (df != nullptr && df->kind() == JVal::kInt) delta = df->as_int(1);
  if (delta <= 0) return kBoundUnk;
  auto resolve = [&](const JVal* f) -> const JVal* {
    if (f == nullptr || f->kind() != JVal::kInt) return nullptr;
    auto it = pos_by_id.find((int)f->as_int(-1));
    if (it == pos_by_id.end()) return nullptr;
    int p = it->second;
    if (p < 0 || p >= (int)nodes.size()) return nullptr;
    return nodes[p];
  };
  const JVal* init = resolve(initf);
  if (init == nullptr) return kBoundUnk;
  int64_t start;
  if (!lit_int_value(init, &start)) return kBoundUnk;
  const JVal* cond = resolve(condf);
  if (cond == nullptr || cond->kind() != JVal::kObj) return kBoundUnk;
  const JVal* op = cond->find("op");
  const JVal* rf = cond->find("r");
  if (op == nullptr || op->kind() != JVal::kStr || rf == nullptr) return kBoundUnk;
  bool inclusive = false;
  std::string ops = op->as_str();
  if (ops == "lt") inclusive = false;
  else if (ops == "le") inclusive = true;
  else return kBoundUnk;
  const JVal* rhs = resolve(rf);
  if (rhs == nullptr) return kBoundUnk;
  int64_t stop;
  if (!lit_int_value(rhs, &stop)) return kBoundUnk;
  int64_t trip;
  if (!inclusive) {
    if (stop <= start) return 0;
    trip = (stop - start + delta - 1) / delta;
  } else {
    if (stop < start) return 0;
    trip = (stop - start) / delta + 1;
  }
  if (trip < 0 || trip > kMaxBoundInt) return kBoundUnk;
  return (int)trip;
}

bool agent_static_steps(const JVal* agent, int* steps) {
  if (agent == nullptr || agent->kind() != JVal::kObj) return false;
  const JVal* ms = agent->find("max_steps");
  if (ms == nullptr || ms->kind() != JVal::kInt) return false;
  int64_t v = ms->as_int(0);
  if (v <= 0) return false;
  *steps = (int)v;
  return true;
}

struct CallPair { int m, t; };

int child_pos(const JVal* n, const char* key, const std::map<int,int>& pos_by_id) {
  if (n == nullptr) return -1;
  const JVal* f = n->find(key);
  if (f == nullptr || f->kind() != JVal::kInt) return -1;
  auto it = pos_by_id.find((int)f->as_int(-1));
  return it == pos_by_id.end() ? -1 : it->second;
}

// ---- max_spans 复算 ----
int span_rec(int pos, const std::vector<const JVal*>& nodes,
             const std::map<int,int>& pos_by_id, std::vector<int>& memo) {
  if (pos < 0 || pos >= (int)nodes.size()) return 0;
  if (memo[pos] != -100) return memo[pos];
  memo[pos] = kBoundUnk;  // 占位防环：真走到占位说明有环，诚实说"不知道"
  const JVal* n = nodes[pos];
  if (n == nullptr || n->kind() != JVal::kObj) { memo[pos] = 0; return 0; }
  const JVal* kf = n->find("k");
  if (kf == nullptr || kf->kind() != JVal::kStr) { memo[pos] = kBoundUnrec; return kBoundUnrec; }
  const std::string k = kf->as_str();
  int v = 0;
  auto cpid = [&](const char* key) -> int { return child_pos(n, key, pos_by_id); };
  if (k == "lit" || k == "var" || k == "interp" || k == "nop") {
    v = 0;
  } else if (k == "field" || k == "index") {
    int of = cpid("of"); if (of >= 0) v = absorb_sum(v, span_rec(of, nodes, pos_by_id, memo));
  } else if (k == "stop") {
    int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo));
    int d = cpid("data"); if (d >= 0) v = absorb_sum(v, span_rec(d, nodes, pos_by_id, memo));
  } else if (k == "mem_txn") {
    int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo));
  } else if (k == "emit") {
    const JVal* args = n->find("args");
    if (args != nullptr && args->kind() == JVal::kArr)
      for (size_t i = 0; i < args->size(); ++i) {
        const JVal* a = args->at(i);
        if (a != nullptr && a->kind() == JVal::kInt) v = absorb_sum(v, span_rec((int)a->as_int(-1), nodes, pos_by_id, memo));
      }
  } else if (k == "on") {
    v = absorb_sum(v, 0);
  } else if (k == "sleep") {
    int ms = cpid("ms"); if (ms >= 0) v = absorb_sum(v, span_rec(ms, nodes, pos_by_id, memo));
  } else if (k == "until") {
    int tm = cpid("timeout_ms"); if (tm >= 0) v = absorb_sum(v, span_rec(tm, nodes, pos_by_id, memo));
  } else if (k == "cast" || k == "unop" || k == "len") {
    int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo));
  } else if (k == "binop") {
    int l = cpid("l"); if (l >= 0) v = absorb_sum(v, span_rec(l, nodes, pos_by_id, memo));
    int r = cpid("r"); if (r >= 0) v = absorb_sum(v, span_rec(r, nodes, pos_by_id, memo));
  } else if (k == "at") {
    int of = cpid("of"); if (of >= 0) v = absorb_sum(v, span_rec(of, nodes, pos_by_id, memo));
    int i = cpid("i"); if (i >= 0) v = absorb_sum(v, span_rec(i, nodes, pos_by_id, memo));
  } else if (k == "array_lit") {
    const JVal* kids = n->find("kids");
    if (kids != nullptr && kids->kind() == JVal::kArr)
      for (size_t i = 0; i < kids->size(); ++i) {
        const JVal* kd = kids->at(i);
        if (kd != nullptr && kd->kind() == JVal::kInt) v = absorb_sum(v, span_rec((int)kd->as_int(-1), nodes, pos_by_id, memo));
      }
  } else if (k == "call_model" || k == "call_tool" || k == "call_fn" || k == "call_native" || k == "store_op") {
    v = 1;
  } else if (k == "decl" || k == "destructure" || k == "assign" || k == "return" || k == "exprstmt") {
    int e = cpid("e"); if (e >= 0) v = absorb_sum(v, span_rec(e, nodes, pos_by_id, memo));
  } else if (k == "seq") {
    const JVal* kids = n->find("kids");
    if (kids != nullptr && kids->kind() == JVal::kArr)
      for (size_t i = 0; i < kids->size(); ++i) {
        const JVal* kd = kids->at(i);
        if (kd != nullptr && kd->kind() == JVal::kInt) v = absorb_sum(v, span_rec((int)kd->as_int(-1), nodes, pos_by_id, memo));
      }
  } else if (k == "if") {
    int best = 0;
    int then = cpid("then"); if (then >= 0) best = absorb_max(best, span_rec(then, nodes, pos_by_id, memo));
    const JVal* elifs = n->find("elifs");
    if (elifs != nullptr && elifs->kind() == JVal::kArr)
      for (size_t i = 0; i < elifs->size(); ++i) {
        const JVal* e = elifs->at(i);
        if (e == nullptr) continue;
        const JVal* b = e->find("body");
        if (b != nullptr && b->kind() == JVal::kInt) best = absorb_max(best, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
    int els = cpid("else"); if (els >= 0) best = absorb_max(best, span_rec(els, nodes, pos_by_id, memo));
    v = absorb_sum(1, best);
  } else if (k == "match") {
    int best = 0;
    const JVal* arms = n->find("arms");
    if (arms != nullptr && arms->kind() == JVal::kArr)
      for (size_t i = 0; i < arms->size(); ++i) {
        const JVal* a = arms->at(i);
        if (a == nullptr) continue;
        const JVal* b = a->find("body");
        if (b != nullptr && b->kind() == JVal::kInt) best = absorb_max(best, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
    int def = cpid("default"); if (def >= 0) best = absorb_max(best, span_rec(def, nodes, pos_by_id, memo));
    v = absorb_sum(1, best);
  } else if (k == "parallel") {
    v = 1;
    const JVal* branches = n->find("branches");
    if (branches != nullptr && branches->kind() == JVal::kArr)
      for (size_t i = 0; i < branches->size(); ++i) {
        const JVal* b = branches->at(i);
        if (b != nullptr && b->kind() == JVal::kInt) v = absorb_sum(v, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
  } else if (k == "retry") {
    int body = cpid("body");
    int bv = body >= 0 ? span_rec(body, nodes, pos_by_id, memo) : 0;
    int attempts = 1;
    const JVal* nf = n->find("n");
    if (nf != nullptr && nf->kind() == JVal::kInt) { int64_t nv = nf->as_int(-1); if (nv >= 0) attempts = (int)nv + 1; }
    v = absorb_sum(1, scaled_bound(bv, attempts));
  } else if (k == "for") {
    int body = cpid("body");
    int bv = body >= 0 ? span_rec(body, nodes, pos_by_id, memo) : 0;
    v = absorb_sum(1, scaled_bound(bv, for_trip_count(pos_by_id, nodes, n)));
  } else if (k == "while") {
    v = kBoundUnk;
  } else if (k == "break" || k == "continue") {
    v = 0;
  } else if (k == "agent") {
    const JVal* agent = n->find("agent");
    int steps;
    if (agent == nullptr || !agent_static_steps(agent, &steps)) { v = kBoundUnk; }
    else {
      int kk = 1;
      const JVal* mpt = agent->find("max_parallel_tools");
      if (mpt != nullptr && mpt->kind() == JVal::kInt) { int64_t kv = mpt->as_int(1); if (kv >= 1) kk = (int)kv; }
      v = (1 + kk) * steps;
    }
  } else if (k == "try") {
    v = 1;
    int body = cpid("body"); if (body >= 0) v = absorb_sum(v, span_rec(body, nodes, pos_by_id, memo));
    int best = 0;
    const JVal* catches = n->find("catches");
    if (catches != nullptr && catches->kind() == JVal::kArr)
      for (size_t i = 0; i < catches->size(); ++i) {
        const JVal* c = catches->at(i);
        if (c == nullptr) continue;
        const JVal* b = c->find("body");
        if (b != nullptr && b->kind() == JVal::kInt) best = absorb_max(best, span_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
    v = absorb_sum(v, best);
  } else if (k == "fallback") {
    v = 1;
    int mainn = cpid("main"); if (mainn >= 0) v = absorb_sum(v, span_rec(mainn, nodes, pos_by_id, memo));
    int body = cpid("body"); if (body >= 0) v = absorb_sum(v, span_rec(body, nodes, pos_by_id, memo));
  } else {
    v = kBoundUnrec;
  }
  memo[pos] = v;
  return v;
}

// ---- max_model_calls / max_tool_calls 复算 ----
CallPair calls_rec(int pos, const std::vector<const JVal*>& nodes,
                   const std::map<int,int>& pos_by_id, std::vector<CallPair>& memo) {
  if (pos < 0 || pos >= (int)nodes.size()) return {0, 0};
  if (memo[pos].m != -100) return memo[pos];
  memo[pos] = {kBoundUnk, kBoundUnk};
  const JVal* n = nodes[pos];
  if (n == nullptr || n->kind() != JVal::kObj) { memo[pos] = {0, 0}; return {0, 0}; }
  const JVal* kf = n->find("k");
  if (kf == nullptr || kf->kind() != JVal::kStr) { memo[pos] = {kBoundUnrec, kBoundUnrec}; return memo[pos]; }
  const std::string k = kf->as_str();
  CallPair r = {0, 0};
  auto cpid = [&](const char* key) -> int { return child_pos(n, key, pos_by_id); };
  auto add = [&](CallPair a, CallPair b) -> CallPair { return { absorb_sum(a.m, b.m), absorb_sum(a.t, b.t) }; };
  auto mx  = [&](CallPair a, CallPair b) -> CallPair { return { absorb_max(a.m, b.m), absorb_max(a.t, b.t) }; };
  auto mul = [&](CallPair a, int64_t nn) -> CallPair { return { scaled_bound(a.m, nn), scaled_bound(a.t, nn) }; };
  if (k == "call_model") r = {1, 0};
  else if (k == "call_tool") r = {0, 1};
  else if (k == "stop" || k == "mem_txn" || k == "emit" || k == "on" || k == "sleep" || k == "until") r = {0, 0};
  else if (k == "lit" || k == "var" || k == "interp" || k == "nop" || k == "break" || k == "continue") r = {0, 0};
  else if (k == "call_fn" || k == "call_native") r = {0, 0};
  else if (k == "store_op") {
    int m = 0;
    const JVal* op = n->find("op");
    if (op != nullptr && op->kind() == JVal::kStr) {
      std::string ops = op->as_str();
      if (ops == "vector.put" || ops == "vector.query") m = 1;
    }
    r = {m, 0};
  }
  else if (k == "field" || k == "index") { int of = cpid("of"); if (of >= 0) r = calls_rec(of, nodes, pos_by_id, memo); }
  else if (k == "cast" || k == "unop" || k == "len") { int e = cpid("e"); if (e >= 0) r = calls_rec(e, nodes, pos_by_id, memo); }
  else if (k == "binop") {
    int l = cpid("l"); if (l >= 0) r = add(calls_rec(l, nodes, pos_by_id, memo), r);
    int rr = cpid("r"); if (rr >= 0) r = add(calls_rec(rr, nodes, pos_by_id, memo), r);
  }
  else if (k == "at") {
    int of = cpid("of"); if (of >= 0) r = add(calls_rec(of, nodes, pos_by_id, memo), r);
    int i = cpid("i"); if (i >= 0) r = add(calls_rec(i, nodes, pos_by_id, memo), r);
  }
  else if (k == "array_lit") {
    const JVal* kids = n->find("kids");
    if (kids != nullptr && kids->kind() == JVal::kArr)
      for (size_t i = 0; i < kids->size(); ++i) {
        const JVal* kd = kids->at(i);
        if (kd != nullptr && kd->kind() == JVal::kInt) r = add(calls_rec((int)kd->as_int(-1), nodes, pos_by_id, memo), r);
      }
  }
  else if (k == "decl" || k == "destructure" || k == "assign" || k == "return" || k == "exprstmt") {
    int e = cpid("e"); if (e >= 0) r = calls_rec(e, nodes, pos_by_id, memo);
  }
  else if (k == "seq") {
    const JVal* kids = n->find("kids");
    if (kids != nullptr && kids->kind() == JVal::kArr)
      for (size_t i = 0; i < kids->size(); ++i) {
        const JVal* kd = kids->at(i);
        if (kd != nullptr && kd->kind() == JVal::kInt) r = add(calls_rec((int)kd->as_int(-1), nodes, pos_by_id, memo), r);
      }
  }
  else if (k == "if") {
    int then = cpid("then"); if (then >= 0) r = calls_rec(then, nodes, pos_by_id, memo);
    const JVal* elifs = n->find("elifs");
    if (elifs != nullptr && elifs->kind() == JVal::kArr)
      for (size_t i = 0; i < elifs->size(); ++i) {
        const JVal* e = elifs->at(i);
        if (e == nullptr) continue;
        const JVal* b = e->find("body");
        if (b != nullptr && b->kind() == JVal::kInt) r = mx(r, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
    int els = cpid("else"); if (els >= 0) r = mx(r, calls_rec(els, nodes, pos_by_id, memo));
  }
  else if (k == "match") {
    const JVal* arms = n->find("arms");
    if (arms != nullptr && arms->kind() == JVal::kArr)
      for (size_t i = 0; i < arms->size(); ++i) {
        const JVal* a = arms->at(i);
        if (a == nullptr) continue;
        const JVal* b = a->find("body");
        if (b != nullptr && b->kind() == JVal::kInt) r = mx(r, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
    int def = cpid("default"); if (def >= 0) r = mx(r, calls_rec(def, nodes, pos_by_id, memo));
  }
  else if (k == "parallel") {
    const JVal* branches = n->find("branches");
    if (branches != nullptr && branches->kind() == JVal::kArr)
      for (size_t i = 0; i < branches->size(); ++i) {
        const JVal* b = branches->at(i);
        if (b != nullptr && b->kind() == JVal::kInt) r = add(r, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
  }
  else if (k == "for") {
    int trip = for_trip_count(pos_by_id, nodes, n);
    if (trip < 0) r = {kBoundUnk, kBoundUnk};
    else {
      int body = cpid("body");
      r = (body >= 0) ? calls_rec(body, nodes, pos_by_id, memo) : CallPair{0, 0};
      r = mul(r, trip);
    }
  }
  else if (k == "while") r = {kBoundUnk, kBoundUnk};
  else if (k == "agent") {
    const JVal* agent = n->find("agent");
    int steps;
    if (agent == nullptr || !agent_static_steps(agent, &steps)) r = {kBoundUnk, kBoundUnk};
    else {
      int kk = 1;
      const JVal* mpt = agent->find("max_parallel_tools");
      if (mpt != nullptr && mpt->kind() == JVal::kInt) { int64_t kv = mpt->as_int(1); if (kv >= 1) kk = (int)kv; }
      r = {steps, kk * steps};
    }
  }
  else if (k == "try") {
    int body = cpid("body"); if (body >= 0) r = calls_rec(body, nodes, pos_by_id, memo);
    CallPair best = {0, 0};
    const JVal* catches = n->find("catches");
    if (catches != nullptr && catches->kind() == JVal::kArr)
      for (size_t i = 0; i < catches->size(); ++i) {
        const JVal* c = catches->at(i);
        if (c == nullptr) continue;
        const JVal* b = c->find("body");
        if (b != nullptr && b->kind() == JVal::kInt) best = mx(best, calls_rec((int)b->as_int(-1), nodes, pos_by_id, memo));
      }
    r = add(r, best);
  }
  else if (k == "retry") {
    int attempts = 1;
    const JVal* nf = n->find("n");
    if (nf != nullptr && nf->kind() == JVal::kInt) { int64_t nv = nf->as_int(-1); if (nv >= 0) attempts = (int)nv + 1; }
    int body = cpid("body");
    if (body >= 0) r = mul(calls_rec(body, nodes, pos_by_id, memo), attempts);
  }
  else if (k == "fallback") {
    int mainn = cpid("main"); if (mainn >= 0) r = add(r, calls_rec(mainn, nodes, pos_by_id, memo));
    int body = cpid("body"); if (body >= 0) r = add(r, calls_rec(body, nodes, pos_by_id, memo));
  }
  else r = {kBoundUnrec, kBoundUnrec};
  memo[pos] = r;
  return r;
}

}  // namespace hm / anonymous (bounds recompute)
}  // namespace hm

#pragma GCC visibility push(default)
namespace hm {
// gather_modules 是"方案 B"的核心：facts 带来各模块的**源码字节**，
// C++ 侧逐模块跑前端，把 `#include` 进来的类型 / backend / tool / 函数
// 合并成 unit 级事实，并拼出跨文件 extern 声明表（ext）。
//
// C++ 仍然**不读文件**（LANGUAGE §1.2）：字节由调用方给。
bool gather_modules(const JVal* modules, std::string* ext, JVal* types, JVal* backends,
                    JVal* tools, JVal* exported, std::string* err) {
  std::map<std::string, std::string> extern_lines;
  size_t index = 0;
  for (size_t i = 0; modules != nullptr && i < modules->size(); ++i) {
    const JVal* m = modules->at(i);
    if (m == nullptr || m->kind() != JVal::kObj) continue;
    const JVal* srcv = m->find("source");
    if (srcv == nullptr || srcv->kind() != JVal::kStr || srcv->as_str().empty()) continue;
    const std::string code = srcv->as_str();
    const JVal* pv = m->find("path");
    const char* path = (pv != nullptr && pv->kind() == JVal::kStr) ? pv->as_str().c_str() : "";
    const std::string fname = base_name(path);
    const JVal* kv = m->find("kind");
    const bool is_header = (kv != nullptr && kv->kind() == JVal::kStr && kv->as_str() == "h");
    const size_t mod_index = index++;

    // 类型表：按 include 到达顺序合并，module 记的是**模块下标**。
    JVal ts;
    if (!call_table_h(hm_codegen_types, reinterpret_cast<const uint8_t*>(code.data()),
                      code.size(), fname.c_str(), is_header, &ts)) {
      *err = "frontend rejected the type table of " + fname;
      return false;
    }
    for (size_t t = 0; t < ts.size(); ++t) {
      const JVal* one = ts.at(t);
      if (one == nullptr || one->kind() != JVal::kObj) continue;
      JVal copy = *one;
      copy.set("module", JVal::integer(static_cast<int64_t>(mod_index)));
      types->push(std::move(copy));
    }

    // workflow 声明：export / entry 写在 .h 上，定义不带（LANGUAGE §3.5）。
    JVal wds;
    if (!call_table_h(hm_codegen_wfdecls, reinterpret_cast<const uint8_t*>(code.data()),
                      code.size(), fname.c_str(), is_header, &wds)) {
      *err = "frontend rejected the workflow declarations of " + fname;
      return false;
    }
    for (size_t w = 0; w < wds.size(); ++w) {
      const JVal* one = wds.at(w);
      if (one == nullptr || one->kind() != JVal::kObj) continue;
      const JVal* ex = one->find("extern");
      if (ex != nullptr && ex->kind() == JVal::kBool && ex->as_bool(false)) {
        // 跨文件 extern 声明：符号名 → 声明行（行由前端格式化好）。
        const JVal* sym = one->find("sym");
        const JVal* line = one->find("ext_line");
        if (sym != nullptr && sym->kind() == JVal::kStr && line != nullptr &&
            line->kind() == JVal::kStr && !sym->as_str().empty() && !line->as_str().empty()) {
          extern_lines[sym->as_str()] = line->as_str();
        }
        continue;
      }
      const JVal* nm = one->find("name");
      if (nm == nullptr || nm->kind() != JVal::kStr || nm->as_str().empty()) continue;
      // **按 OR 合并**：定义里 false、声明里 true 时必须是 true
      // （entry 必须 exported），先到先得会把声明那一份丢掉。
      const std::string name = nm->as_str();
      bool prev_exp = false, prev_entry = false;
      if (const JVal* prev = exported->find(name)) {
        if (const JVal* pe = prev->find("exported")) {
          if (pe->kind() == JVal::kBool) prev_exp = pe->as_bool(false);
        }
        if (const JVal* pi = prev->find("is_entry")) {
          if (pi->kind() == JVal::kBool) prev_entry = pi->as_bool(false);
        }
      }
      auto flag = [&](const char* key) {
        const JVal* f = one->find(key);
        return f != nullptr && f->kind() == JVal::kBool && f->as_bool(false);
      };
      JVal e = JVal::object();
      e.set("exported", JVal::boolean(prev_exp || flag("exported")));
      e.set("is_entry", JVal::boolean(prev_entry || flag("is_entry")));
      exported->set(name, std::move(e));
    }

    JVal bds;
    if (!call_table_h(hm_codegen_backends, reinterpret_cast<const uint8_t*>(code.data()),
                      code.size(), fname.c_str(), is_header, &bds)) {
      *err = "frontend rejected the backend table of " + fname;
      return false;
    }
    for (size_t b = 0; b < bds.size(); ++b) {
      const JVal* one = bds.at(b);
      if (one != nullptr && one->kind() == JVal::kObj) backends->push(*one);
    }

    JVal tds;
    if (!call_table_h(hm_codegen_tools, reinterpret_cast<const uint8_t*>(code.data()),
                      code.size(), fname.c_str(), is_header, &tds)) {
      *err = "frontend rejected the tool table of " + fname;
      return false;
    }
    for (size_t t = 0; t < tds.size(); ++t) {
      const JVal* one = tds.at(t);
      if (one != nullptr && one->kind() == JVal::kObj) tools->push(*one);
    }
  }

  // ext：先所有 struct 字段行（合并顺序），再 extern 声明行（按符号字典序）。
  // 顺序必须确定：注入顺序会进入入口 AST 的 child 链，进而影响 extern 表建成
  // 顺序与诊断顺序 —— 同一份源码两次编译必须逐字节相同。
  for (size_t t = 0; t < types->size(); ++t) {
    const JVal* one = types->at(t);
    const JVal* nm = one ? one->find("name") : nullptr;
    const JVal* fields = one ? one->find("fields") : nullptr;
    if (nm == nullptr || nm->kind() != JVal::kStr || fields == nullptr) continue;
    for (size_t f = 0; f < fields->size(); ++f) {
      const JVal* fld = fields->at(f);
      const JVal* fn = fld ? fld->find("n") : nullptr;
      const JVal* ft = fld ? fld->find("t") : nullptr;
      if (fn == nullptr || ft == nullptr) continue;
      const JVal* fk = ft->find("k");
      const char* kk = (fk != nullptr && fk->kind() == JVal::kStr) ? fk->as_str().c_str() : "";
      std::string type_name = kk;
      if (type_name == "struct") {
        const JVal* ref = ft->find("ref");
        if (ref != nullptr && ref->kind() == JVal::kStr) type_name = ref->as_str();
      }
      *ext += nm->as_str() + "\t" + fn->as_str() + "\t" + type_name + "\n";
    }
  }
  for (const auto& kv : extern_lines) {
    *ext += kv.second;
    *ext += "\n";
  }
  return true;
}

}  // namespace hm

namespace hm {
// backfill_decl 给一组 owner（workflow 或 function）补 vars[].decl。
// root_fallback：函数表里的形参没有 decl 节点，Go 侧把它们的 decl 也绑到 root
// （与 workflow 的 input/参数同一条语义：入口处由调用方赋值）。workflow 主体
// **不开**这条回落 —— 那里出现"找不到 binder"是真问题，应交回校验器报错，
// 而不是悄悄绑到 root 把裂痕盖住。
void backfill_decl(const JVal& owners, bool root_fallback) {
  // ---- 补全 vars[].decl (owner = workflow 或 function) ---------------------------------------------------
  // 前端不知道"哪个节点绑定了这个变量"（它按函数局部编号），装配层要扫一遍
  // 节点表把 decl 填成那个 decl 节点的 id；找不到就是 -1。
  // 漏了这一步，产物里每个变量都挂 -1 —— IR 依然合法，但依赖 decl 的
  // 分析（存活期、死代码）全部失准。
  //
  // **函数表同样要过这一趟**：函数里的合成变量（如内联产生的 __hm_inline_*）
  // 也在自己的 nodes 里被 decl 节点绑定。只处理 workflow 会让函数体里的每个
  // 变量都挂 -1。
  if (owners.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < owners.size(); ++wi) {
      const JVal* w = owners.at(wi);
      if (w == nullptr || w->kind() != JVal::kObj) continue;
      const JVal* nodes = w->find("nodes");
      const JVal* vars = w->find("vars");
      if (nodes == nullptr || vars == nullptr) continue;
      std::vector<std::pair<int64_t, int64_t>> decl_of;
      for (size_t ni = 0; ni < nodes->size(); ++ni) {
        const JVal* n = nodes->at(ni);
        if (n == nullptr || n->kind() != JVal::kObj) continue;
        const JVal* k = n->find("k");
        const JVal* var = n->find("var");
        const JVal* id = n->find("id");
        if (k == nullptr || k->kind() != JVal::kStr) continue;
        if (id == nullptr || id->kind() != JVal::kInt) continue;
        const std::string kk = k->as_str();
        // 绑定变量的节点有三种：`decl`（单绑定）、`for`（循环变量）、
        // `destructure`（解构位）。只认 decl 会让循环变量与解构变量全挂 -1。
        if (kk == "decl" || kk == "for") {
          if (var == nullptr || var->kind() != JVal::kInt) continue;
          decl_of.push_back({var->as_int(-1), id->as_int(-1)});
          continue;
        }
        if (kk == "try") {
          // catch 绑定的错误对象（`catch(e)`）也由这个 try 节点绑定。
          const JVal* cs = n->find("catches");
          if (cs == nullptr || cs->kind() != JVal::kArr) continue;
          for (size_t cj = 0; cj < cs->size(); ++cj) {
            const JVal* one = cs->at(cj);
            const JVal* cv = one ? one->find("v") : nullptr;
            if (cv == nullptr || cv->kind() != JVal::kInt) continue;
            decl_of.push_back({cv->as_int(-1), id->as_int(-1)});
          }
          continue;
        }
        if (kk == "destructure") {
          const JVal* vs = n->find("vars");
          if (vs == nullptr || vs->kind() != JVal::kArr) continue;
          for (size_t vj = 0; vj < vs->size(); ++vj) {
            const JVal* one = vs->at(vj);
            if (one == nullptr || one->kind() != JVal::kInt) continue;  // null = `_` 丢弃位
            decl_of.push_back({one->as_int(-1), id->as_int(-1)});
          }
        }
      }
      for (size_t vi = 0; vi < vars->size(); ++vi) {
        const JVal* v = vars->at(vi);
        if (v == nullptr || v->kind() != JVal::kObj) continue;
        const JVal* id = v->find("id");
        if (id == nullptr || id->kind() != JVal::kInt) continue;
        int64_t at = -1;
        for (const auto& kv : decl_of) {
          if (kv.first == id->as_int(-1)) { at = kv.second; break; }
        }
        // 入口变量 `input` 没有 decl 节点：它在 workflow 入口由调用方赋值，
        // 所以 decl 就是 root。漏了这一条，所有入口参数都挂 -1，而依赖 decl
        // 的分析（存活期、死代码）会把它们当成"从未被绑定"。
        if (at < 0) {
          const JVal* nm = v->find("name");
          const JVal* rootv = w->find("root");
          const bool named_input =
              nm != nullptr && nm->kind() == JVal::kStr && nm->as_str() == "input";
          if ((named_input || root_fallback) && rootv != nullptr && rootv->kind() == JVal::kInt) {
            at = rootv->as_int(0);
          }
        }
        const_cast<JVal*>(v)->set("decl", JVal::integer(at));
      }
    }
  }

}

}  // namespace hm

extern "C" int hm_ir_assemble_final(const uint8_t* src, size_t len, const char* filename,
                                    const char* facts_json, size_t facts_len,
                                    char* out, size_t cap, size_t* out_len,
                                    char* out_err, size_t err_cap, size_t* err_len) {
  using namespace hm;
  if (src == nullptr) {
    put("hm_ir_assemble_final: nil source", out_err, err_cap, err_len);
    return 2;
  }

  size_t pos = 0;  // 复用于 facts 与前端输出的解析
  JVal facts = JVal::object();
  if (facts_json != nullptr && facts_len > 0) {
    if (!parse_json(facts_json, facts_len, &facts, &pos)) {
      put("IR assembly: facts_json is not valid JSON", out_err, err_cap, err_len);
      return 2;
    }
  }
  JVal modules = JVal::array();
  {
    const JVal* m = facts.find("modules");
    if (m != nullptr) modules = *m;
  }
  // 模块 path 也只留文件名：与 Go 侧一致（IR 不携带目录结构）。
  if (modules.kind() == JVal::kArr) {
    JVal fixed = JVal::array();
    for (size_t i = 0; i < modules.size(); ++i) {
      const JVal* m = modules.at(i);
      if (m == nullptr || m->kind() != JVal::kObj) { fixed.push(JVal::null()); continue; }
      JVal copy = JVal::object();
      const char* mp = nullptr;
      {
        const JVal* pv = m->find("path");
        if (pv != nullptr && pv->kind() == JVal::kStr) mp = pv->as_str().c_str();
      }
      const JVal* b = m->find("builtin");
      copy.set("builtin", b != nullptr ? *b : JVal::boolean(false));
      // path 原样保留：facts 里的模块表来自 Go 侧的 include 解析结果，
      // 已经是"相对 include root 的 POSIX 路径"（IR.md §9.2）。把它改成
      // basename 会让 `models/order.h` 变成 `order.h` —— 两个不同的文件
      // 撞成同一个名字，且产物不再可重现。
      copy.set("path", JVal::string(mp));
      const JVal* sh = m->find("sha256");
      if (sh != nullptr) copy.set("sha256", *sh);
      // source / kind 是"调用方给的外部事实"，装配要用它跑各模块前端。
      // 规范化只改 path（只留文件名），**不能顺手把这两样丢掉** ——
      // 丢了之后 gather 会以为每个模块都没有源码，多文件装配静默退回单文件。
      const JVal* sv = m->find("source");
      if (sv != nullptr) copy.set("source", *sv);
      const JVal* kd = m->find("kind");
      if (kd != nullptr) copy.set("kind", *kd);
      fixed.push(std::move(copy));
    }
    modules = std::move(fixed);
  }

  // ---- 多模块 gather（方案 B）-----------------------------------------
  // facts 带来各模块源码字节时，装配层自己跑一遍各模块的前端调用，把
  // `#include` 进来的类型 / backend / tool / 函数合并成 unit 级事实。
  // 单模块（没有 source）走原来的单文件路径，行为不变。
  bool multi = false;
  std::string ext;
  JVal g_types = JVal::array();
  JVal g_backends = JVal::array();
  JVal g_tools = JVal::array();
  JVal g_functions = JVal::array();
  JVal g_exported = JVal::object();
  {
    size_t with_src = 0;
    if (modules.kind() == JVal::kArr) {
      for (size_t i = 0; i < modules.size(); ++i) {
        const JVal* m = modules.at(i);
        const JVal* sv = m ? m->find("source") : nullptr;
        if (sv != nullptr && sv->kind() == JVal::kStr && !sv->as_str().empty()) ++with_src;
      }
    }
    if (with_src > 1) {
      multi = true;
      std::string gerr;
      if (!gather_modules(&modules, &ext, &g_types, &g_backends, &g_tools, &g_exported, &gerr)) {
        put("IR assembly: " + gerr, out_err, err_cap, err_len);
        return 2;
      }
      for (size_t i = 0; i < modules.size(); ++i) {
        const JVal* m = modules.at(i);
        const JVal* sv = m ? m->find("source") : nullptr;
        if (sv == nullptr || sv->kind() != JVal::kStr || sv->as_str().empty()) continue;
        const JVal* pv = m->find("path");
        const char* path = (pv != nullptr && pv->kind() == JVal::kStr) ? pv->as_str().c_str() : "";
        JVal fs;
        if (!call_functions(reinterpret_cast<const uint8_t*>(sv->as_str().data()),
                            sv->as_str().size(), base_name(path).c_str(), ext, &fs)) {
          put("IR assembly: frontend rejected the function table", out_err, err_cap, err_len);
          return 2;
        }
        for (size_t f = 0; f < fs.size(); ++f) {
          const JVal* one = fs.at(f);
          if (one != nullptr && one->kind() == JVal::kObj) g_functions.push(*one);
        }
      }
    }
  }

  std::string wf_json, diag;
  const int frc = call_frontend(src, len, filename, ext, &wf_json, &diag);
  if (frc == 2) {
    put("IR assembly: frontend buffer negotiation failed", out_err, err_cap, err_len);
    return 2;
  }
  if (frc != 0) {
    // 源码本身编不过：这不是"未覆盖"，是正常的源码错误 —— 交回调用方报诊断。
    put(diag.empty() ? "IR assembly: frontend rejected the source" : diag,
        out_err, err_cap, err_len);
    if (out_len) *out_len = 0;
    return 2;
  }

  JVal workflows;
  if (!parse_json(wf_json.data(), wf_json.size(), &workflows, &pos)) {
    put("IR assembly: frontend output is not valid JSON", out_err, err_cap, err_len);
    return 2;
  }
  // 多文件：`export` / `entry` 写在 `.h` 的**声明**上，定义不带（LANGUAGE §3.5）。
  // 不贴回去，entry workflow 的 exported 会是 false，exports.workflows 直接空掉。
  if (multi && g_exported.kind() == JVal::kObj && workflows.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows.size(); ++wi) {
      JVal* w = const_cast<JVal*>(workflows.at(wi));
      if (w == nullptr) continue;
      const JVal* nm = w->find("name");
      if (nm == nullptr || nm->kind() != JVal::kStr) continue;
      const JVal* d = g_exported.find(nm->as_str());
      if (d == nullptr) continue;
      const JVal* ex = d->find("exported");
      if (ex != nullptr) w->set("exported", *ex);
      const JVal* en = d->find("is_entry");
      if (en != nullptr) w->set("is_entry", *en);
    }
  }

  // exports.workflows 是**全部 exported workflow，按声明顺序**。
  // 单 workflow 的源码看起来像"只登记入口"，但那只是巧合 —— 多 workflow
  // 的源码（functions.hermes）会列出全部 export 的。
  JVal exported_workflows = JVal::array();
  if (workflows.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows.size(); ++wi) {
      const JVal* w = workflows.at(wi);
      const JVal* ex = w ? w->find("exported") : nullptr;
      const JVal* nm = w ? w->find("name") : nullptr;
      if (ex != nullptr && ex->kind() == JVal::kBool && ex->as_bool(false) && nm != nullptr &&
          nm->kind() == JVal::kStr) {
        exported_workflows.push(JVal::string(nm->as_str()));
      }
    }
  }

  backfill_decl(workflows, false);

  JVal types = JVal::array();
  if (multi) {
    // 多模块：类型表已由 gather_modules 按 include 顺序合并。
    types = g_types;
  } else {
    std::string types_json;
    if (call_types(src, len, filename, &types_json) != 0) {
      put("IR assembly: frontend rejected the type table", out_err, err_cap, err_len);
      return 2;
    }
    size_t tpos = 0;
    if (!types_json.empty() && !parse_json(types_json.data(), types_json.size(), &types, &tpos)) {
      put("IR assembly: type table is not valid JSON", out_err, err_cap, err_len);
      return 2;
    }
  }
  // 类型表：分配 id、解析 ref、算内容摘要（顺序不能换）。
  {
    std::string terr;
    if (!assign_type_ids(&types, &terr)) {
      put("IR assembly: " + terr, out_err, err_cap, err_len);
      return 2;
    }
  }

  JVal schemas;
  {
    std::string serr;
    if (!build_schemas(&types, &schemas, &serr)) {
      put("IR assembly: " + serr, out_err, err_cap, err_len);
      return 2;
    }
  }

  // backends 来自前端的 backend 表（声明与定义两形态，装配期按名字合并前先对齐形状）。
  JVal backends_list = JVal::array();
  if (multi) {
    backends_list = g_backends;
  } else if (!call_table(hm_codegen_backends, src, len, filename, &backends_list, "backends")) {
    put("IR assembly: frontend rejected the backend table", out_err, err_cap, err_len);
    return 2;
  }
  // ---- backends 形状对齐 ---------------------------------------------------
  // 前端给的是 AST 原貌（`loc` + `decl_form`），IR 的对外字段是
  // `decl_loc` / `def_loc` 两个**位置不同含义**的键（声明处 vs 定义处）。
  // 直接搬 AST 会把"这个 backend 定义在哪"变成没人认识的 `loc`，
  // 而链接期要靠 decl_loc/def_loc 判断声明与定义是否配对。
  if (backends_list.kind() == JVal::kArr) {
    JVal aligned = JVal::array();
    for (size_t i = 0; i < backends_list.size(); ++i) {
      const JVal* b = backends_list.at(i);
      if (b == nullptr || b->kind() != JVal::kObj) continue;
      JVal e = JVal::object();
      std::vector<std::string> keys;
      b->keys(&keys);
      std::string decl_form;
      for (const std::string& k : keys) {
        const JVal* v = b->find(k);
        if (v == nullptr) continue;
        if (k == "decl_form") {
          if (v->kind() == JVal::kStr) decl_form = v->as_str();
          continue;
        }
        if (k == "loc") {
          // 判据是 `decl_form == "sig"`（声明形态）：sig → 声明处，其余 → 定义处。
          // 写死成 "decl" 会让所有 backend 的位置都落到 def_loc，声明与定义
          // 在链接期就无法配对。
          e.set(decl_form == "sig" ? "decl_loc" : "def_loc", *v);
          continue;
        }
        e.set(k, *v);
      }
      aligned.push(std::move(e));
    }
    backends_list = std::move(aligned);
  }

  // ---- tools[] 合成 -------------------------------------------------------
  // 工具**不是**从 `export tool` 表来的，而是从每个 `call_tool` 节点自带的
  // `tool_decl` 里合成出来的（名字形如 "tool@0:8:14"）。合成完要把 tool_decl
  // 从节点上**删掉** —— 否则同一个工具的事实会同时出现在节点和 tools[] 里。
  JVal tools_synth = JVal::array();
  {
    std::vector<std::string> seen;
    // ① `export tool` 表：它是工具的另一条来源（agent 用 tools 列表引用它们）。
    //    前端只给声明信息，IR 的 Tool 还要 allow_status / idem / timeout_ms ——
    //    这三样是**默认值**，必须补齐，否则下游读不到"没有 allow_status"这件事。
    JVal declared = multi ? g_tools
                          : JVal::array();
    if (multi || call_table(hm_codegen_tools, src, len, filename, &declared, "tools")) {
      for (size_t i = 0; i < declared.size(); ++i) {
        const JVal* t0 = declared.at(i);
        if (t0 == nullptr || t0->kind() != JVal::kObj) continue;
        JVal t = *t0;
        if (t.find("allow_status") == nullptr) t.set("allow_status", JVal::null());
        if (t.find("idem") == nullptr) {
          JVal idem = JVal::object();
          idem.set("mode", JVal::string(""));
          t.set("idem", std::move(idem));
        }
        if (t.find("timeout_ms") == nullptr) t.set("timeout_ms", JVal::integer(0));
        const JVal* m = t.find("method");
        if (m == nullptr || (m->kind() == JVal::kStr && m->as_str().empty())) {
          t.set("method", JVal::string("GET"));
        }
        const JVal* nm = t.find("name");
        const std::string tname = (nm != nullptr && nm->kind() == JVal::kStr) ? nm->as_str() : "";
        bool dup = false;
        for (const std::string& s2 : seen) { if (s2 == tname) { dup = true; break; } }
        if (!dup) {
          seen.push_back(tname);
          tools_synth.push(std::move(t));
        }
      }
    }
    if (workflows.kind() == JVal::kArr) {
      for (size_t wi = 0; wi < workflows.size(); ++wi) {
        const JVal* w = workflows.at(wi);
        const JVal* nodes = w ? w->find("nodes") : nullptr;
        if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
        for (size_t ni = 0; ni < nodes->size(); ++ni) {
          JVal* n = const_cast<JVal*>(nodes->at(ni));
          if (n == nullptr) continue;
          const JVal* k = n->find("k");
          if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "call_tool") continue;
          JVal* decl = const_cast<JVal*>(n->find("tool_decl"));
          if (decl == nullptr) {
            put("IR assembly: call_tool node is missing tool_decl", out_err, err_cap, err_len);
            return 2;
          }
          if (decl->kind() == JVal::kObj) {
            JVal t = *decl;
            const JVal* m = t.find("method");
            if (m == nullptr || (m->kind() == JVal::kStr && m->as_str().empty())) {
              t.set("method", JVal::string("GET"));
            }
            const JVal* nm = t.find("name");
            const std::string tname = (nm != nullptr && nm->kind() == JVal::kStr) ? nm->as_str() : "";
            bool dup = false;
            for (const std::string& s2 : seen) { if (s2 == tname) { dup = true; break; } }
            if (!dup) {
              seen.push_back(tname);
              tools_synth.push(std::move(t));
            }
          }
          drop_key(n, "tool_decl");
        }
      }
    }
  }

  // functions[] 是**函数表**：跨 workflow 可见，所以是 unit 级的一份，
  // 不塞进任何单个 workflow（否则同一份函数会出现多次）。
  JVal functions_list = JVal::array();
  if (multi) {
    functions_list = g_functions;
  } else if (!call_functions(src, len, filename, ext, &functions_list)) {
    put("IR assembly: frontend rejected the function table", out_err, err_cap, err_len);
    return 2;
  }
  // 函数体里的节点同样要归一化（与 workflow 节点同一套规则）。
  if (functions_list.kind() == JVal::kArr) {
    for (size_t fi = 0; fi < functions_list.size(); ++fi) {
      const JVal* fn = functions_list.at(fi);
      const JVal* nodes = fn ? fn->find("nodes") : nullptr;
      if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
      for (size_t ni = 0; ni < nodes->size(); ++ni) {
        JVal* n = const_cast<JVal*>(nodes->at(ni));
        if (n == nullptr) continue;
        for (const char* key : {"ret", "to"}) {
          JVal* v = const_cast<JVal*>(n->find(key));
          if (v != nullptr) normalize_type_expr(v, &types);
        }
        prune_empty(*n);
      }
    }
  }
  // 函数体内的合成变量（如内联产生的 __hm_inline_*）也要把 decl 指到绑定节点。
  backfill_decl(functions_list, true);

  // ---- backends 按名字合并 -------------------------------------------------
  // kind 与 exported 来自 `.h` 的**声明**，addr 来自 `.hermes` 的**定义**；
  // 两者按名字合成一条。不合并会出现两条同名 backend —— 一条只有地址、
  // 一条只有类型，而 `workflow.backend = "g"` 的名字引用会指向哪条都不对。
  if (backends_list.kind() == JVal::kArr) {
    JVal merged = JVal::array();
    std::vector<std::string> order;
    std::vector<JVal> by_name;
    for (size_t i = 0; i < backends_list.size(); ++i) {
      const JVal* b = backends_list.at(i);
      if (b == nullptr || b->kind() != JVal::kObj) continue;
      const JVal* nm = b->find("name");
      if (nm == nullptr || nm->kind() != JVal::kStr) continue;
      const std::string name = nm->as_str();
      size_t at = order.size();
      for (size_t k = 0; k < order.size(); ++k) {
        if (order[k] == name) { at = k; break; }
      }
      if (at == order.size()) {
        order.push_back(name);
        JVal e = JVal::object();
        e.set("name", JVal::string(name));
        e.set("kind", JVal::string(""));
        e.set("exported", JVal::boolean(false));
        by_name.push_back(std::move(e));
      }
      JVal& e = by_name[at];
      const JVal* kind = b->find("kind");
      if (kind != nullptr && kind->kind() == JVal::kStr && !kind->as_str().empty()) {
        e.set("kind", *kind);
      }
      const JVal* ex = b->find("exported");
      if (ex != nullptr && ex->kind() == JVal::kBool && ex->as_bool(false)) {
        e.set("exported", JVal::boolean(true));
      }
      const JVal* addr = b->find("addr");
      if (addr != nullptr) e.set("addr", *addr);
      const JVal* dl = b->find("decl_loc");
      if (dl != nullptr) e.set("decl_loc", *dl);
      const JVal* fl = b->find("def_loc");
      if (fl != nullptr) e.set("def_loc", *fl);
    }
    backends_list = std::move(merged);
    for (size_t i = 0; i < by_name.size(); ++i) backends_list.push(std::move(by_name[i]));
  }

  auto exported_names = [](const JVal& list) {
    JVal out = JVal::array();
    if (list.kind() != JVal::kArr) return out;
    for (size_t i = 0; i < list.size(); ++i) {
      const JVal* e = list.at(i);
      const JVal* ex = e ? e->find("exported") : nullptr;
      const JVal* nm = e ? e->find("name") : nullptr;
      if (ex != nullptr && ex->kind() == JVal::kBool && ex->as_bool(false) && nm != nullptr &&
          nm->kind() == JVal::kStr) {
        out.push(JVal::string(nm->as_str()));
      }
    }
    return out;
  };
  // exports.backends / exports.tools 在今天的装配里**恒为空**：导出面只登记
  // workflows 与 types（internal/ir/assemble.go:212）。这里照抄那个事实，
  // 而不是"看起来更合理"地去收集名字 —— 产物字节不认合理性，只认事实。
  JVal exported_tools = JVal::array();
  JVal exported_backends = JVal::array();
  (void)exported_names;

  JVal exported_types = JVal::array();
  if (types.kind() == JVal::kArr) {
    for (size_t i = 0; i < types.size(); ++i) {
      const JVal* t = types.at(i);
      const JVal* ex = t ? t->find("exported") : nullptr;
      const JVal* nm = t ? t->find("name") : nullptr;
      if (ex != nullptr && ex->kind() == JVal::kBool && ex->as_bool(false) && nm != nullptr &&
          nm->kind() == JVal::kStr) {
        exported_types.push(JVal::string(nm->as_str()));
      }
    }
  }

  // ---- 归一化：丢掉 null 与空数组 -----------------------------------------
  // 前端按"所见即所在"输出（`elifs:[]`、`else:null` 都写在里面），而 Go 的
  // Doc/Node 结构体用 omitempty —— 空切片与 nil 指针都不会出现在产物里。
  // 装配层负责把形状对齐：只丢 null 与空数组；空字符串与 0 **保留**
  // （它们在 Go 侧是指针/显式值，omitempty 不会丢）。
  // 只对 **node 对象**做归一化：workflow 级字段（`params` / `input` /
  // `interp_deps`）在 Go 里没有 omitempty，即使为空也要写出来。
  // 同一个键名在不同结构体上可能一个有 omitempty 一个没有 —— 所以按**位置**
  // 而不是按键名套规则，是这里唯一正确的粒度。
  // ---- call_tool 的返回类型闸门 -----------------------------------------
  // `call_tool` 的 T 必须是 JSON 结构类型；double 是标量。放行它会静默产出
  // call_tool.ret={"k":"double"} —— emitExpr 不读 ret，C 后端照编不误，
  // 前端/装配/ir.Load/C 后端没有**任何一层**会拒，产物却 schema-invalid。
  // 诊断文案与既有回归测试逐字一致（那条测试断言的就是这句话）。
  if (workflows.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows.size(); ++wi) {
      const JVal* w = workflows.at(wi);
      const JVal* nodes = w ? w->find("nodes") : nullptr;
      if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
      for (size_t ni = 0; ni < nodes->size(); ++ni) {
        const JVal* n = nodes->at(ni);
        const JVal* k = n ? n->find("k") : nullptr;
        if (k == nullptr || k->kind() != JVal::kStr || k->as_str() != "call_tool") continue;
        const JVal* ret = n->find("ret");
        const JVal* rk = ret ? ret->find("k") : nullptr;
        if (rk != nullptr && rk->kind() == JVal::kStr && rk->as_str() == "double") {
          put("ir.AssembleUnit: return type \"double\" is undeclared (typo?)", out_err, err_cap, err_len);
          if (out_len) *out_len = 0;
          return 2;
        }
      }
    }
  }

  // 节点上的类型表达式（ret / to / vars.t）先规范成 IR 形状。
  if (workflows.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows.size(); ++wi) {
      const JVal* w = workflows.at(wi);
      const JVal* nodes = w ? w->find("nodes") : nullptr;
      if (nodes != nullptr && nodes->kind() == JVal::kArr) {
        for (size_t ni = 0; ni < nodes->size(); ++ni) {
          JVal* n = const_cast<JVal*>(nodes->at(ni));
          if (n == nullptr) continue;
          for (const char* key : {"ret", "to"}) {
            JVal* v = const_cast<JVal*>(n->find(key));
            if (v != nullptr) normalize_type_expr(v, &types);
          }
        }
      }
      const JVal* vars0 = w ? w->find("vars") : nullptr;
      if (vars0 != nullptr && vars0->kind() == JVal::kArr) {
        for (size_t vi = 0; vi < vars0->size(); ++vi) {
          JVal* v = const_cast<JVal*>(vars0->at(vi));
          JVal* t = v ? const_cast<JVal*>(v->find("t")) : nullptr;
          if (t != nullptr) normalize_type_expr(t, &types);
        }
      }
    }
  }

  // call_model 的 `schema_ref` 是类型**名字**；IR 的对外字段是 `schema`（下标）。
  // 这一步必须在 node 归一化之前做：归一化会按 omitempty 丢掉 schema_ref。
  if (workflows.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows.size(); ++wi) {
      const JVal* w = workflows.at(wi);
      const JVal* nodes = w ? w->find("nodes") : nullptr;
      if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
      for (size_t ni = 0; ni < nodes->size(); ++ni) {
        JVal* n = const_cast<JVal*>(nodes->at(ni));
        if (n == nullptr) continue;
        JVal* ref = const_cast<JVal*>(n->find("schema_ref"));
        if (ref == nullptr) continue;
        // 没有 schema 的 call_model 是 `schema_ref: null`：IR 的对外形状是
        // `schema: null`，键名要换、值要留 —— 直接跳过会把它留成 schema_ref。
        if (ref->kind() == JVal::kNull) {
          n->set("schema", JVal::null());
          drop_key(n, "schema_ref");
          continue;
        }
        if (ref->kind() != JVal::kStr) continue;
        int64_t idx = -1;
        for (size_t si = 0; si < schemas.size(); ++si) {
          const JVal* sd = schemas.at(si);
          const JVal* root = sd ? sd->find("root") : nullptr;
          if (root != nullptr && root->kind() == JVal::kStr && root->as_str() == ref->as_str()) {
            idx = static_cast<int64_t>(si);
            break;
          }
        }
        n->set("schema", JVal::integer(idx));
        // schema_ref 用完必须删：留着等于同一个事实在产物里出现两遍。
        drop_key(n, "schema_ref");
      }
    }
  }

  if (workflows.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows.size(); ++wi) {
      const JVal* w = workflows.at(wi);
      const JVal* nodes = w ? w->find("nodes") : nullptr;
      if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
      for (size_t ni = 0; ni < nodes->size(); ++ni) {
        JVal* n = const_cast<JVal*>(nodes->at(ni));
        if (n != nullptr) prune_empty(*n);
      }
    }
  }

  // ---- 静态上界复算（装配层第二份实现，镜像 internal/ir/bounds.go）--------
  // 在每个 workflow 节点的形状完全定型之后，按 IR.md §5.1 的代数从根重算
  // max_spans / max_model_calls / max_tool_calls，覆盖回 workflow 的 bounds。
  // codegen 在**前端**已经写了一份 bounds（max_steps / max_concurrency /
  // max_retry_expansion / unbounded 是装配期不掌握的计数器，沿用前端值）；
  // 这里只覆盖代数三字段：
  //   - 复算返回 -1（kBoundUnk，合法地算不出，如 while / 无界 agent）时**回退到
  //     codegen 原值**，保证含 unbounded 的基线字节不变；
  //   - 复算返回 -2（kBoundUnrec，编译器 bug：碰到不认识的节点种类）原样写出，
  //     让字节比对立刻暴露没跟上的新 kind，而不是静默通过。
  if (workflows.kind() == JVal::kArr) {
    for (size_t wi = 0; wi < workflows.size(); ++wi) {
      JVal* w = const_cast<JVal*>(workflows.at(wi));
      if (w == nullptr || w->kind() != JVal::kObj) continue;
      const JVal* nodesv = w->find("nodes");
      if (nodesv == nullptr || nodesv->kind() != JVal::kArr) continue;
      const size_t nn = nodesv->size();
      std::vector<const JVal*> nodes(nn);
      std::map<int,int> pos_by_id;
      for (size_t i = 0; i < nn; ++i) {
        const JVal* n = nodesv->at(i);
        nodes[i] = n;
        if (n != nullptr && n->kind() == JVal::kObj) {
          const JVal* idf = n->find("id");
          if (idf != nullptr && idf->kind() == JVal::kInt) {
            pos_by_id[(int)idf->as_int(-1)] = (int)i;
          }
        }
      }
      const JVal* rootv = w->find("root");
      int root_pos = -1;
      if (rootv != nullptr && rootv->kind() == JVal::kInt) {
        auto it = pos_by_id.find((int)rootv->as_int(-1));
        if (it != pos_by_id.end()) root_pos = it->second;
      }
      if (root_pos < 0) continue;  // 没有根，无法定上界；跳过（不应发生）

      std::vector<int> span_memo(nn, -100);
      std::vector<CallPair> call_memo(nn, {-100, -100});
      const int ms = span_rec(root_pos, nodes, pos_by_id, span_memo);
      const CallPair cp = calls_rec(root_pos, nodes, pos_by_id, call_memo);

      // 契约子句（D-036）：`result` 是合成变量，没有 decl 节点可推类型，
      // 装配期按 workflow 的 returns 回填。不回填它就会以 json 名义参与比较，
      // 而 `ensure(result != "")` 在运行期会变成"json 与 string 比较"——
      // 出错的样子与真实原因完全无关。
      {
        const JVal* rv = w->find("contract_result_var");
        const JVal* ret = w->find("returns");
        JVal* vars = const_cast<JVal*>(w->find("vars"));
        if (rv != nullptr && rv->kind() == JVal::kInt && ret != nullptr &&
            ret->kind() == JVal::kObj && vars != nullptr && vars->kind() == JVal::kArr) {
          const int64_t idx = rv->as_int(-1);
          for (size_t vi = 0; vi < vars->size(); ++vi) {
            JVal* v = const_cast<JVal*>(vars->at(vi));
            if (v == nullptr) continue;
            const JVal* idf = v->find("id");
            if (idf == nullptr || idf->kind() != JVal::kInt || idf->as_int(-1) != idx) continue;
            JVal t = JVal::object();
            const JVal* k = ret->find("k");
            if (k != nullptr) t.set("k", *k);
            const JVal* id2 = ret->find("id");
            if (id2 != nullptr) t.set("id", *id2);
            v->set("t", std::move(t));
            break;
          }
        }
      }

      JVal* bounds = const_cast<JVal*>(w->find("bounds"));
      if (bounds == nullptr || bounds->kind() != JVal::kObj) {
        w->set("bounds", JVal::object());
        bounds = const_cast<JVal*>(w->find("bounds"));
      }
      if (bounds == nullptr) continue;
      // 覆盖 max_spans：复算 -1（unknown）则保留 codegen 原值。
      if (ms != kBoundUnk) bounds->set("max_spans", JVal::integer((int64_t)ms));
      // 覆盖 max_model_calls / max_tool_calls：复算 -1 则保留 codegen 原值。
      if (cp.m != kBoundUnk) bounds->set("max_model_calls", JVal::integer((int64_t)cp.m));
      if (cp.t != kBoundUnk) bounds->set("max_tool_calls", JVal::integer((int64_t)cp.t));
    }
  }

  JVal doc = JVal::object();
  doc.set("backends", std::move(backends_list));
  {
    JVal c = JVal::object();
    c.set("name", JVal::string("hermes-parse"));
    c.set("parser_abi", JVal::integer(1));
    c.set("version", JVal::string(HERMES_IR_COMPILER_VERSION));
    doc.set("compiler", std::move(c));
  }
  {
    JVal d = JVal::object();
    d.set("error", JVal::integer(0));
    d.set("warning", JVal::integer(0));
    doc.set("diag_counts", std::move(d));
  }
  {
    JVal e = JVal::object();
    e.set("backends", std::move(exported_backends));
    e.set("tools", std::move(exported_tools));
    e.set("types", std::move(exported_types));
    e.set("workflows", std::move(exported_workflows));
    doc.set("exports", std::move(e));
  }
  // imports[] 自己算：它完全由 call_native 节点决定（符号 + 首次出现的位置），
  // 不需要调用方喂。用 facts 里的值会让"装配"依赖 Go 的另一份推导。
  {
    JVal imports = JVal::array();
    std::vector<std::string> seen_sym;
    auto scan = [&](const JVal& owners) {
      if (owners.kind() != JVal::kArr) return;
      for (size_t wi = 0; wi < owners.size(); ++wi) {
        const JVal* w = owners.at(wi);
        const JVal* nodes = w ? w->find("nodes") : nullptr;
        if (nodes == nullptr || nodes->kind() != JVal::kArr) continue;
        for (size_t ni = 0; ni < nodes->size(); ++ni) {
          const JVal* n = nodes->at(ni);
          const JVal* kv = n ? n->find("k") : nullptr;
          if (kv == nullptr || kv->kind() != JVal::kStr || kv->as_str() != "call_native") continue;
          const JVal* fv = n->find("fn");
          if (fv == nullptr || fv->kind() != JVal::kStr) continue;
          const std::string fn = fv->as_str();
          bool dup = false;
          for (const std::string& s2 : seen_sym) { if (s2 == fn) { dup = true; break; } }
          if (dup) continue;
          seen_sym.push_back(fn);
          JVal one = JVal::object();
          one.set("kind", JVal::string("native"));
          const JVal* loc = n->find("loc");
          if (loc != nullptr) one.set("loc", *loc);
          one.set("sym", JVal::string(fn));
          imports.push(std::move(one));
        }
      }
    };
    scan(workflows);
    scan(functions_list);
    doc.set("imports", std::move(imports));
  }
  doc.set("ir_version", JVal::integer(2));
  {
    const JVal* r = facts.find("requires");
    doc.set("requires", r != nullptr ? *r : JVal::array());
  }
  doc.set("schemas", std::move(schemas));
  doc.set("tools", std::move(tools_synth));
  doc.set("types", std::move(types));
  {
    JVal u = JVal::object();
    // entry_path 只存**文件名**（不含目录）：IR 是可存档的产物，绝对路径
    // 会把打包机的目录结构带进每一个产物，也让"同一份源码两份字节"。
    u.set("entry_path", JVal::string(base_name(filename)));
    u.set("language_version", JVal::integer(2));
    // unit.modules 只承载 IR 的身份三项（builtin / path / sha256）：
    // source 与 kind 是**调用方给装配用的外部事实**，属于编译器输入，
    // 不进产物 —— 否则 IR 会随源码内容膨胀，且泄露非契约字段。
    JVal unit_mods = JVal::array();
    if (modules.kind() == JVal::kArr) {
      for (size_t i = 0; i < modules.size(); ++i) {
        const JVal* m = modules.at(i);
        if (m == nullptr || m->kind() != JVal::kObj) continue;
        JVal one = JVal::object();
        for (const char* key : {"builtin", "path", "sha256"}) {
          const JVal* v = m->find(key);
          if (v != nullptr) one.set(key, *v);
        }
        unit_mods.push(std::move(one));
      }
    }
    u.set("modules", std::move(unit_mods));
    u.set("name", JVal::string(unit_name(filename)));
    doc.set("unit", std::move(u));
  }
  // functions 为空时**不写这个键**（基线如此）：它是函数表，没有函数时
  // 出现一个空数组只会让下游多一次"有没有函数"的判断。
  if (functions_list.kind() == JVal::kArr && functions_list.size() > 0) {
    doc.set("functions", std::move(functions_list));
  }
  if (!workflows.empty_obj() || workflows.kind() == JVal::kArr) {
    // 键按字节序排序由 canonical 写出器负责，这里只管语义。
    doc.set("workflows", std::move(workflows));
  }

  const std::string text = doc.str();
  if (!put(text, out, cap, out_len)) {
    put("IR assembly: output buffer too small", out_err, err_cap, err_len);
    return 2;
  }
  if (err_len) *err_len = 0;
  return 0;
}
#pragma GCC visibility pop
