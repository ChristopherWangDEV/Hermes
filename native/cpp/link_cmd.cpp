// link_cmd.cpp —— `hermes link`：把多个 `.hbc` 合成一个可执行归档（IR.md §9）。
//
// **状态：未完工（2.0.2）**。本文件实现完了符号解析（Link），还差 VerifyRequires /
// Merge / Reach / CLI。它**尚未加入 scripts/build.sh**，所以不参与构建、不影响
// 已发布的 2.0.1。留在这里而不是 /tmp，是为了让"搬了一半"这件事有迹可循，
// 而不是躺在某个临时目录里等人重新发现。
//
// 它比"把文件拼起来"多做的三件事：符号解析（重名是错误）、ODR、依赖字节校验。
// 这三件都不做的话，产物能跑，但"跑的是谁"没人说得清。
//
// **每一句错误文案都与 Go 版逐字相同**：判据是 tests/link/golden.json 里冻结的
// 8 个用例（退出码 / stdout / stderr / 产物 sha256）。那些 golden 是在 Go 实现
// 还存在时录下来的——搬错不会报错，这类命令只能靠冻结的期望值证明"搬对了"。
#include "hermes_parser.h"
#include "json_write.h"
#include "std_variants.h"

// 前端已有的 sha256（同一份实现，不引第二个哈希）。
extern "C" int hm_sha256(const unsigned char* data, size_t len, unsigned char* out);
extern "C" int hm_std_source(const char* name, char* out, size_t cap, size_t* out_len);

#include <sys/stat.h>

#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <map>
#include <set>
#include <string>
#include <vector>
#include <cstdio>

static bool read_file_for_link(const char* path, std::string* out) {
  FILE* f = std::fopen(path, "rb");
  if (f == nullptr) return false;
  char buf[16384];
  size_t n;
  out->clear();
  while ((n = std::fread(buf, 1, sizeof buf, f)) > 0) out->append(buf, n);
  std::fclose(f);
  return true;
}

namespace {

constexpr int kABIVersionSupported = 2;

struct Member {
  std::string name;
  std::string ir;
  std::string ir_sha;
  // 原始索引项：Merge 要原样复制 exports / requires / 版本字段，
  // 而不是从 IR 重新推导——推导出来的可能与索引里记的不一致，而索引才是权威。
  hm::JVal index;
};

struct Input {
  std::string path;
  std::string raw;
  std::vector<Member> members;
  std::string index_sha;
};

std::string jstr(const hm::JVal* o, const char* k) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
}
long long jint(const hm::JVal* o, const char* k, long long d) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return (v != nullptr && v->kind() == hm::JVal::kInt) ? v->as_int(d) : d;
}
bool jbool(const hm::JVal* o, const char* k) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return v != nullptr && v->kind() == hm::JVal::kBool && v->as_bool(false);
}

// type_name 与 explain 的同一函数同形（struct#id、a<b>）。
std::string type_name(const hm::JVal* t) {
  if (t == nullptr || t->kind() != hm::JVal::kObj) return "unknown";
  std::string name = jstr(t, "k");
  if (name.empty()) name = "unknown";
  if (name == "struct") {
    const hm::JVal* id = t->find("id");
    if (id != nullptr && id->kind() == hm::JVal::kInt) {
      char b[32];
      std::snprintf(b, sizeof b, "struct#%lld", static_cast<long long>(id->as_int(0)));
      name = b;
    }
  }
  const hm::JVal* of = t->find("of");
  if (of != nullptr && of->kind() == hm::JVal::kObj) name = name + "<" + type_name(of) + ">";
  return name;
}

std::string short_sha(const std::string& s) { return s.size() <= 16 ? s : s.substr(0, 16); }

std::string split_field_name(const std::string& s) {
  const size_t pos = s.find(" : ");
  return pos == std::string::npos ? s : s.substr(0, pos);
}
std::string split_field_type(const std::string& s) {
  const size_t pos = s.find(" : ");
  return pos == std::string::npos ? "" : s.substr(pos + 3);
}

std::vector<std::string> diff_fields(const std::vector<std::string>& a,
                                     const std::vector<std::string>& b) {
  std::map<std::string, std::string> am, bm;
  std::vector<std::string> order;
  for (const std::string& s : a) {
    const std::string n = split_field_name(s);
    am[n] = split_field_type(s);
    order.push_back(n);
  }
  for (const std::string& s : b) {
    const std::string n = split_field_name(s);
    bm[n] = split_field_type(s);
    if (am.find(n) == am.end()) order.push_back(n);
  }
  std::vector<std::string> out;
  std::set<std::string> seen;
  for (const std::string& n : order) {
    if (!seen.insert(n).second) continue;
    const bool aok = am.count(n) > 0, bok = bm.count(n) > 0;
    if (aok && bok && am[n] != bm[n]) {
      out.push_back("field " + n + ": " + am[n] + " != " + bm[n]);
    } else if (aok && !bok) {
      out.push_back("field " + n + " exists only in the first definition");
    } else if (!aok && bok) {
      out.push_back("field " + n + " exists only in the second definition");
    }
  }
  if (out.empty()) out.push_back("same field set but different sha256: check annotations and optionality");
  return out;
}

struct TypeRecord {
  std::string sha, member, lib;
  std::vector<std::string> fields;
};

std::string odr_message(const std::string& name, const TypeRecord& a, const TypeRecord& b) {
  std::string s = "struct " + name + " is defined differently in two places (E_LINK_ODR_MISMATCH):\n";
  s += "  member " + a.lib + ":" + a.member + "\n";
  s += "  member " + b.lib + ":" + b.member + "\n";
  for (const std::string& d : diff_fields(a.fields, b.fields)) s += "  " + d + "\n";
  s += "  = why: the two libraries disagree on the struct behind this name. If one is silently chosen, the other library's generated JSON Schema no longer matches its own reader code --\n";
  s += "             at runtime this shows up as 'fields silently disappearing', with no way to trace it.\n";
  s += "  = fix: rename one of the structs, or make both libraries depend on the same .h";
  return s;
}

bool load_input(const std::string& path, Input* in) {
  in->path = path;
  if (!read_file_for_link(path.c_str(), &in->raw)) return false;
  std::vector<char> mj(1 << 16);
  size_t mn = 0;
  int rc = hm_archive_read(reinterpret_cast<const unsigned char*>(in->raw.data()), in->raw.size(),
                           mj.data(), mj.size(), &mn);
  if (rc == -4 && mn > mj.size()) {
    mj.resize(mn);
    rc = hm_archive_read(reinterpret_cast<const unsigned char*>(in->raw.data()), in->raw.size(),
                         mj.data(), mj.size(), &mn);
  }
  if (rc != 0) return false;
  char hex[65] = {0};
  if (hm_archive_index_sha256(reinterpret_cast<const unsigned char*>(in->raw.data()),
                              in->raw.size(), hex) != 0) {
    return false;
  }
  in->index_sha = hex;
  hm::JVal arr;
  size_t pos = 0;
  if (!hm::parse_json(mj.data(), mn > 0 ? mn - 1 : 0, &arr, &pos) || arr.kind() != hm::JVal::kArr) {
    return false;
  }
  for (size_t i = 0; i < arr.size(); ++i) {
    const hm::JVal* m = arr.at(i);
    if (m == nullptr) continue;
    Member mem;
    mem.name = jstr(m, "name");
    const size_t off = static_cast<size_t>(jint(m, "ir_off", 0));
    const size_t len = static_cast<size_t>(jint(m, "ir_len", 0));
    if (off + len > in->raw.size()) return false;
    mem.ir.assign(in->raw.data() + off, len);
    mem.ir_sha = jstr(m, "ir_sha256");
    mem.index = *m;
    in->members.push_back(mem);
  }
  return true;
}

// validate_ir 调 C++ 校验器，返回 0 通过 / 2 IR 非法 / 1 读不动。
int validate_ir(const std::string& ir) {
  std::vector<char> dg(1 << 16), er(256);
  size_t dn = 0, en = 0;
  char errbuf[256] = {0};
  const int rc = hm_ir_validate(reinterpret_cast<const unsigned char*>(ir.data()), ir.size(),
                                dg.data(), dg.size(), &dn, errbuf, sizeof errbuf, &en);
  if (rc != 0) return 1;
  // out_len 是**实际写入字节数**（不含 NUL）：合法 IR 时是 2（"[]"）。
  // 早先按"含 NUL"取 dn-1，于是把 "[]" 读成 "["，把每一份合法归档判成非法。
  const std::string ds(dg.data(), dn);
  return (ds == "[]" || ds.empty()) ? 0 : 2;
}

}  // namespace

namespace {

struct Sym {
  std::string kind, name, member, lib;
};

struct LinkResult {
  std::vector<std::string> members;                                  // lib.member
  std::vector<std::string> entries;
  std::map<std::string, std::map<std::string, Sym>> symbols;         // kind → name → sym
};

// 各 kind 的规范化名字（Go 的 Kind 是字符串常量）。
const char* kWorkflow = "workflow";
const char* kType = "type";
const char* kBackend = "backend";
const char* kTool = "tool";

std::string upper(std::string s) {
  for (char& c : s) {
    if (c >= 'a' && c <= 'z') c = static_cast<char>(c - 'a' + 'A');
  }
  return s;
}

// doc_has_symbol：本 unit 自己导出的符号也算"已解析"。
bool doc_has_symbol(const hm::JVal* doc, const std::string& kind, const std::string& sym) {
  if (kind == kWorkflow) {
    const hm::JVal* a = doc->find("workflows");
    if (a != nullptr && a->kind() == hm::JVal::kArr) {
      for (size_t i = 0; i < a->size(); ++i) {
        const hm::JVal* w = a->at(i);
        if (jstr(w, "name") == sym && jbool(w, "exported")) return true;
      }
    }
  } else if (kind == kType) {
    const hm::JVal* a = doc->find("types");
    if (a != nullptr && a->kind() == hm::JVal::kArr) {
      for (size_t i = 0; i < a->size(); ++i) {
        const hm::JVal* t = a->at(i);
        if (jstr(t, "name") == sym) return true;
      }
    }
  } else if (kind == kBackend) {
    const hm::JVal* a = doc->find("backends");
    if (a != nullptr && a->kind() == hm::JVal::kArr) {
      for (size_t i = 0; i < a->size(); ++i) {
        if (jstr(a->at(i), "name") == sym) return true;
      }
    }
  } else if (kind == kTool) {
    const hm::JVal* a = doc->find("tools");
    if (a != nullptr && a->kind() == hm::JVal::kArr) {
      for (size_t i = 0; i < a->size(); ++i) {
        if (jstr(a->at(i), "name") == sym) return true;
      }
    }
  }
  return false;
}

bool has_native_require(const hm::JVal* doc) {
  const hm::JVal* a = doc->find("requires");
  if (a == nullptr || a->kind() != hm::JVal::kArr) return false;
  for (size_t i = 0; i < a->size(); ++i) {
    if (jstr(a->at(i), "kind") == "native") return true;
  }
  return false;
}

std::vector<std::string> field_summary(const hm::JVal* t) {
  std::vector<std::string> out;
  const hm::JVal* f = (t != nullptr) ? t->find("fields") : nullptr;
  if (f == nullptr || f->kind() != hm::JVal::kArr) return out;
  for (size_t i = 0; i < f->size(); ++i) {
    const hm::JVal* fd = f->at(i);
    out.push_back(jstr(fd, "n") + " : " + type_name(fd->find("t")));
  }
  return out;
}

// link_all 实现 Go 的 Link()：符号解析 + ODR + imports + native + 版本。
LinkResult link_all(const std::vector<Input>& inputs, std::vector<std::string>* problems) {
  LinkResult res;
  res.symbols[kWorkflow] = {};
  res.symbols[kType] = {};
  res.symbols[kBackend] = {};
  res.symbols[kTool] = {};
  std::map<std::string, std::map<std::string, Sym>> seen;
  seen[kWorkflow] = {};
  seen[kType] = {};
  seen[kBackend] = {};
  seen[kTool] = {};
  std::map<std::string, TypeRecord> type_defs;

  for (const Input& in : inputs) {
    for (const Member& m : in.members) {
      res.members.push_back(in.path + "." + m.name);
      hm::JVal doc;
      size_t pos = 0;
      if (!hm::parse_json(m.ir.data(), m.ir.size(), &doc, &pos) ||
          doc.kind() != hm::JVal::kObj) {
        problems->push_back(in.path + ": cannot validate IR of member \"" + m.name +
                            "\": IR is not readable");
        continue;
      }
      const int vr = validate_ir(m.ir);
      if (vr != 0) {
        if (vr == 2) {
          problems->push_back(in.path + ": IR of member \"" + m.name +
                              "\" is invalid (1 diagnostics); it must pass load-time validation "
                              "before linking");
        }
        continue;
      }

      // ---- 类型：ODR ----
      const hm::JVal* types = doc.find("types");
      if (types != nullptr && types->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < types->size(); ++i) {
          const hm::JVal* t = types->at(i);
          if (!jbool(t, "exported")) continue;
          const std::string name = jstr(t, "name");
          const std::string sha = jstr(t, "sha256");
          auto it = type_defs.find(name);
          if (it != type_defs.end()) {
            if (it->second.sha != sha) {
              TypeRecord b;
              b.sha = sha;
              b.member = m.name;
              b.lib = in.path;
              b.fields = field_summary(t);
              problems->push_back(odr_message(name, it->second, b));
            }
            continue;
          }
          TypeRecord rec;
          rec.sha = sha;
          rec.member = m.name;
          rec.lib = in.path;
          rec.fields = field_summary(t);
          type_defs[name] = rec;
          Sym sy;
          sy.kind = kType;
          sy.name = name;
          sy.member = m.name;
          sy.lib = in.path;
          res.symbols[kType][name] = sy;
        }
      }

      // ---- workflow：同名即错 ----
      const hm::JVal* wfs = doc.find("workflows");
      if (wfs != nullptr && wfs->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < wfs->size(); ++i) {
          const hm::JVal* w = wfs->at(i);
          if (!jbool(w, "exported")) continue;
          const std::string name = jstr(w, "name");
          auto it = seen[kWorkflow].find(name);
          if (it != seen[kWorkflow].end()) {
            problems->push_back("duplicate workflow \"" + name + "\": " + it->second.lib + "/" +
                                it->second.member + " and " + in.path + "/" + m.name +
                                ". Not overwriting and not continuing with a warning: a silent "
                                "overwrite would make 'what did I just link' unknowable "
                                "(E_LINK_DUP_WORKFLOW)");
            continue;
          }
          Sym sy;
          sy.kind = kWorkflow;
          sy.name = name;
          sy.member = m.name;
          sy.lib = in.path;
          seen[kWorkflow][name] = sy;
          res.symbols[kWorkflow][name] = sy;
          if (jbool(w, "is_entry")) res.entries.push_back(name);
        }
      }

      // ---- backend / tool：同名即错 ----
      auto check_dup = [&](const char* kind, const std::string& name) {
        auto it = seen[kind].find(name);
        if (it != seen[kind].end()) {
          problems->push_back(std::string("duplicate ") + kind + " \"" + name + "\": both " +
                              it->second.lib + "/" + it->second.member + " and " + in.path + "/" +
                              m.name + " export it (E_LINK_DUP_" + upper(kind) +
                              "); if the two libraries configure the same name differently, there "
                              "is no answer to which one takes effect after linking");
          return;
        }
        Sym sy;
        sy.kind = kind;
        sy.name = name;
        sy.member = m.name;
        sy.lib = in.path;
        seen[kind][name] = sy;
        res.symbols[kind][name] = sy;
      };
      const hm::JVal* bes = doc.find("backends");
      if (bes != nullptr && bes->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < bes->size(); ++i) {
          const hm::JVal* b = bes->at(i);
          if (jbool(b, "exported")) check_dup(kBackend, jstr(b, "name"));
        }
      }
      const hm::JVal* tls = doc.find("tools");
      if (tls != nullptr && tls->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < tls->size(); ++i) check_dup(kTool, jstr(tls->at(i), "name"));
      }

      // ---- imports ----
      const hm::JVal* ims = doc.find("imports");
      if (ims != nullptr && ims->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < ims->size(); ++i) {
          const hm::JVal* im = ims->at(i);
          const std::string kind = jstr(im, "kind");
          const std::string sym = jstr(im, "sym");
          if (kind == "native") continue;
          auto table = res.symbols.find(kind);
          if (table == res.symbols.end()) continue;
          if (table->second.count(sym) > 0) continue;
          if (doc_has_symbol(&doc, kind, sym)) continue;
          const hm::JVal* loc = im->find("loc");
          problems->push_back("unresolved symbol \"" + sym + "\" (" + kind +
                              "), referenced by " + in.path + "/" + m.name + " at L" +
                              std::to_string(jint(loc, "l", 0)) +
                              " -- add the library that provides it (-l), or confirm it belongs in "
                              "this unit instead (E_LINK_UNRESOLVED)");
        }
      }

      // ---- native 符号 ----
      const hm::JVal* nodes_all = doc.find("workflows");
      if (nodes_all != nullptr && !has_native_require(&doc)) {
        // 只对真的调用了 native 的成员报（call_native 节点）。
        std::vector<std::pair<std::string, long long>> calls;
        if (nodes_all->kind() == hm::JVal::kArr) {
          for (size_t wi = 0; wi < nodes_all->size(); ++wi) {
            const hm::JVal* w = nodes_all->at(wi);
            const hm::JVal* ns = (w != nullptr) ? w->find("nodes") : nullptr;
            if (ns == nullptr || ns->kind() != hm::JVal::kArr) continue;
            for (size_t ni = 0; ni < ns->size(); ++ni) {
              const hm::JVal* n = ns->at(ni);
              if (n == nullptr || jstr(n, "k") != "call_native") continue;
              const hm::JVal* loc = n->find("loc");
              calls.emplace_back(jstr(n, "sym"), jint(loc, "l", 0));
            }
          }
        }
        for (const auto& c : calls) {
          problems->push_back("unresolved native symbol \"" + c.first + "\", referenced by " +
                              in.path + "/" + m.name + " at L" + std::to_string(c.second) +
                              " -- this unit calls a native (C) function but declares no `#link "
                              "native \"...\" from \"...\"`. linking a native call requires a "
                              "matching native require (E_LINK_UNRESOLVED)");
        }
      }

      // ---- requires 版本 ----
      const hm::JVal* reqs = doc.find("requires");
      const long long doc_ir_version = jint(&doc, "ir_version", 2);
      if (reqs != nullptr && reqs->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < reqs->size(); ++i) {
          const hm::JVal* r = reqs->at(i);
          if (jbool(r, "implicit")) continue;
          const long long abi = jint(r, "abi_version", 0);
          if (abi != kABIVersionSupported) {
            problems->push_back(m.name + " requires " + jstr(r, "spec") +
                                " with abi_version=" + std::to_string(abi) +
                                ", but this linker only accepts " +
                                std::to_string(kABIVersionSupported) +
                                ". ABI versions must match **exactly** (not just major): a minor "
                                "change can alter struct layout, causing memory to be "
                                "misinterpreted (E_LINK_ABI_MISMATCH)");
          }
          const long long irv = jint(r, "ir_version", 0);
          if (irv != doc_ir_version) {
            std::string why = "the linked library speaks a different IR major version";
            if (irv > doc_ir_version) why = "the linked library is newer than the unit that links it";
            problems->push_back(m.name + " requires " + jstr(r, "spec") +
                                " with ir_version=" + std::to_string(irv) +
                                ", but this unit links at ir_version=" +
                                std::to_string(doc_ir_version) + ": " + why +
                                ". IR major versions must match, and a dependency must never be "
                                "newer than its linker (E_LINK_IR_MISMATCH)");
          }
        }
      }
    }
  }
  std::sort(res.members.begin(), res.members.end());
  std::sort(res.entries.begin(), res.entries.end());
  return res;
}

}  // namespace

namespace {

// std_impl_exists 判断"选中的变体下，该成员是否有实现"。
// 表由 tools/genstdvariants 从 internal/std 生成——手抄会漂移，
// 而漂移的症状是"链接期放过了运行期才炸的依赖"。
bool std_impl_exists(const std::string& mod, const std::string& variant) {
  for (size_t i = 0; i < hm::kStdVariantCount; ++i) {
    const hm::StdVariantRow& r = hm::kStdVariants[i];
    if (mod == r.mod && variant == r.variant) return true;
  }
  return false;
}

std::string selected_variant() {
  const char* v = std::getenv("HERMES_STD_VARIANT");
  if (v != nullptr && (std::strcmp(v, "c") == 0 || std::strcmp(v, "C") == 0)) return "c";
  return "go";
}

// std_module_of 从 `<std/net>` 取出 `net`。
std::string std_module_of(const std::string& spec) {
  std::string s = spec;
  if (!s.empty() && s.front() == '<') s = s.substr(1);
  if (!s.empty() && s.back() == '>') s.pop_back();
  if (s.rfind("std/", 0) == 0) s = s.substr(4);
  return s;
}

// sha256_hex_of 对一段文本算 sha256（走前端已有的 hm_sha256）。
std::string sha256_hex_of(const std::string& s) {
  unsigned char sum[32] = {0};
  if (hm_sha256(reinterpret_cast<const unsigned char*>(s.data()), s.size(), sum) != 0) return "";
  static const char* kHex = "0123456789abcdef";
  std::string out;
  out.reserve(64);
  for (int i = 0; i < 32; ++i) {
    out.push_back(kHex[sum[i] >> 4]);
    out.push_back(kHex[sum[i] & 0xf]);
  }
  return out;
}

// verify_std_require 与 Go 的 verifyStdRequire 逐字对应。
bool verify_std_require(const std::string& spec, const std::string& want, std::string* err) {
  const std::string mod = std_module_of(spec);
  if (mod == spec || mod.empty()) {
    *err = "spec \"" + spec + "\" is not of the form <std/...>";
    return false;
  }
  std::vector<char> buf(1 << 16);
  size_t n = 0;
  int rc = hm_std_source(mod.c_str(), buf.data(), buf.size(), &n);
  if (rc == -4 && n > buf.size()) {
    buf.resize(n);
    rc = hm_std_source(mod.c_str(), buf.data(), buf.size(), &n);
  }
  if (rc != 0 || n <= 1) {
    *err = "no module \"" + mod +
           "\" in the embedded stdlib (IR depends on a module this version does not provide)";
    return false;
  }
  const std::string src(buf.data(), n - 1);
  const std::string got = sha256_hex_of(src);
  if (got != want) {
    *err = "member_sha256 mismatch: IR claims dependency " + short_sha(want) +
           "..., but module \"" + mod + "\" in the embedded stdlib is " + short_sha(got) +
           "... (E_LINK_TAMPERED)";
    return false;
  }
  return true;
}

// verify_local_require：链接输入里必须有一份归档，其索引哈希等于 member_sha256。
bool verify_local_require(const std::string& want, const std::string& spec,
                          const std::vector<Input>& inputs, std::string* err) {
  if (want.empty()) {
    *err =
        "requires is missing member_sha256: a dependency is declared without naming its exact "
        "bytes (IR.md §9.5) (E_LINK_REQUIRES_INCOMPLETE)";
    return false;
  }
  for (const Input& in : inputs) {
    if (in.index_sha == want) return true;
  }
  *err = "no archive with member_sha256 " + short_sha(want) +
         "... among the link inputs (dependency " + spec +
         "; this value is the index hash of the depended-on archive) (E_LINK_TAMPERED)";
  return false;
}

// verify_requires 实现 Go 的 VerifyRequires。
std::vector<std::string> verify_requires(const std::vector<Input>& inputs) {
  std::vector<std::string> problems;
  const std::string variant = selected_variant();
  for (const Input& in : inputs) {
    for (const Member& m : in.members) {
      hm::JVal doc;
      size_t pos = 0;
      if (!hm::parse_json(m.ir.data(), m.ir.size(), &doc, &pos) || doc.kind() != hm::JVal::kObj) {
        problems.push_back(in.path + ": cannot validate IR of member \"" + m.name +
                           "\": IR is not readable");
        continue;
      }
      const int vr = validate_ir(m.ir);
      if (vr != 0) continue;  // 已在 Link 里报过
      const hm::JVal* reqs = doc.find("requires");
      if (reqs == nullptr || reqs->kind() != hm::JVal::kArr) continue;
      for (size_t i = 0; i < reqs->size(); ++i) {
        const hm::JVal* r = reqs->at(i);
        if (jbool(r, "implicit")) continue;
        const std::string kind = jstr(r, "kind");
        const std::string spec = jstr(r, "spec");
        const std::string want = jstr(r, "member_sha256");
        std::string err;
        if (kind == "std") {
          if (!verify_std_require(spec, want, &err)) {
            problems.push_back(in.path + ": std dependency validation failed for member \"" +
                               m.name + "\": " + err);
          }
          const std::string mod = std_module_of(spec);
          if (!std_impl_exists(mod, variant)) {
            problems.push_back(in.path + ": std member \"" + mod +
                               "\" has no implementation for selected variant \"" + variant +
                               "\" (E_LINK_STD_VARIANT)");
          }
        } else if (kind == "native") {
          continue;
        } else {
          if (!verify_local_require(want, spec, inputs, &err)) {
            problems.push_back(in.path + ": local dependency validation failed for member \"" +
                               m.name + "\": " + err);
          }
        }
      }
    }
  }
  return problems;
}

}  // namespace

namespace {

// merge_members 实现 Go 的 Merge：按输入顺序拼成员，**不做死代码剥离**。
// entry 以 exports.workflows 非空为准（与 Go 一致）。
hm::JVal merge_members(const std::vector<Input>& inputs) {
  hm::JVal out = hm::JVal::array();
  for (const Input& in : inputs) {
    for (const Member& m : in.members) {
      hm::JVal o = hm::JVal::object();
      o.set("name", hm::JVal::string(m.name));
      o.set("ir", hm::JVal::string(m.ir));
      const hm::JVal* exp = m.index.find("exports");
      hm::JVal ex = hm::JVal::object();
      bool has_workflows = false;
      if (exp != nullptr && exp->kind() == hm::JVal::kObj) {
        for (const char* k : {"workflows", "types", "backends", "tools"}) {
          const hm::JVal* v = exp->find(k);
          if (v != nullptr && v->kind() == hm::JVal::kArr) {
            ex.set(k, *v);
            if (std::strcmp(k, "workflows") == 0 && v->size() > 0) has_workflows = true;
          } else {
            ex.set(k, hm::JVal::array());
          }
        }
      } else {
        for (const char* k : {"workflows", "types", "backends", "tools"}) ex.set(k, hm::JVal::array());
      }
      o.set("exports", std::move(ex));
      const hm::JVal* reqs = m.index.find("requires");
      o.set("requires", (reqs != nullptr && reqs->kind() == hm::JVal::kArr) ? *reqs
                                                                          : hm::JVal::array());
      o.set("entry", hm::JVal::boolean(has_workflows));
      o.set("language_version", hm::JVal::integer(jint(&m.index, "language_version", 0)));
      o.set("abi_version", hm::JVal::integer(jint(&m.index, "abi_version", 0)));
      out.push(std::move(o));
    }
  }
  return out;
}

// reach 实现 Go 的 Reach：从 entry 出发做不动点，返回保留集合与丢弃列表。
struct ReachResult {
  std::set<std::string> kept;
  std::vector<std::string> dropped;
  std::vector<std::string> problems;
};

ReachResult reach(const std::vector<Input>& inputs) {
  ReachResult r;
  std::map<std::string, std::string> sym_owner;             // kind:name → member
  std::set<std::string> entries;                            // kind:name
  std::map<std::string, std::vector<std::string>> needs;    // member → deps
  std::set<std::string> must_keep;
  std::set<std::string> all_members;

  auto type_symbols = [](const hm::JVal* doc, const hm::JVal* t) -> std::vector<std::string> {
    std::vector<std::string> out;
    if (t == nullptr || t->kind() != hm::JVal::kObj) return out;
    if (jstr(t, "k") != "struct") return out;
    const hm::JVal* id = t->find("id");
    if (id == nullptr || id->kind() != hm::JVal::kInt) return out;
    const hm::JVal* ts = doc->find("types");
    if (ts == nullptr || ts->kind() != hm::JVal::kArr) return out;
    for (size_t i = 0; i < ts->size(); ++i) {
      const hm::JVal* td = ts->at(i);
      if (jint(td, "id", -1) == id->as_int(-1)) out.push_back("type:" + jstr(td, "name"));
    }
    return out;
  };

  for (const Input& in : inputs) {
    for (const Member& m : in.members) {
      const std::string key = m.name;
      all_members.insert(key);
      hm::JVal doc;
      size_t pos = 0;
      if (!hm::parse_json(m.ir.data(), m.ir.size(), &doc, &pos) || doc.kind() != hm::JVal::kObj) {
        r.problems.push_back(in.path + ": cannot validate IR of member \"" + m.name +
                             "\": IR is not readable");
        must_keep.insert(m.name);
        continue;
      }
      if (validate_ir(m.ir) != 0) {
        r.problems.push_back(in.path + ": IR of member \"" + m.name +
                             "\" is invalid and cannot take part in reachability analysis -> "
                             "keeping it conservatively (not stripped)");
        must_keep.insert(m.name);
        continue;
      }
      const hm::JVal* wfs = doc.find("workflows");
      if (wfs != nullptr && wfs->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < wfs->size(); ++i) {
          const hm::JVal* w = wfs->at(i);
          if (!jbool(w, "exported")) continue;
          const std::string sym = "workflow:" + jstr(w, "name");
          sym_owner[sym] = key;
          if (jbool(w, "is_entry")) entries.insert(sym);
        }
      }
      const hm::JVal* ts = doc.find("types");
      if (ts != nullptr && ts->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < ts->size(); ++i) {
          const hm::JVal* t = ts->at(i);
          if (jbool(t, "exported")) sym_owner["type:" + jstr(t, "name")] = key;
        }
      }
      const hm::JVal* bs = doc.find("backends");
      if (bs != nullptr && bs->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < bs->size(); ++i) {
          const hm::JVal* b = bs->at(i);
          if (jbool(b, "exported")) sym_owner["backend:" + jstr(b, "name")] = key;
        }
      }
      std::vector<std::string> deps;
      const hm::JVal* ims = doc.find("imports");
      if (ims != nullptr && ims->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < ims->size(); ++i) {
          const hm::JVal* im = ims->at(i);
          deps.push_back(jstr(im, "kind") + ":" + jstr(im, "sym"));
        }
      }
      if (wfs != nullptr && wfs->kind() == hm::JVal::kArr) {
        for (size_t i = 0; i < wfs->size(); ++i) {
          const hm::JVal* w = wfs->at(i);
          const std::string be = jstr(w, "backend");
          if (!be.empty()) deps.push_back("backend:" + be);
          for (const std::string& s2 : type_symbols(&doc, w->find("input"))) deps.push_back(s2);
          for (const std::string& s2 : type_symbols(&doc, w->find("returns"))) deps.push_back(s2);
        }
      }
      needs[key] = deps;
    }
  }

  std::vector<std::string> queue;
  for (const std::string& sym : entries) {
    auto it = sym_owner.find(sym);
    if (it != sym_owner.end()) queue.push_back(it->second);
  }
  for (size_t qi = 0; qi < queue.size(); ++qi) {
    const std::string key = queue[qi];
    if (!r.kept.insert(key).second) continue;
    auto it = needs.find(key);
    if (it == needs.end()) continue;
    for (const std::string& sym : it->second) {
      auto own = sym_owner.find(sym);
      if (own != sym_owner.end() && r.kept.count(own->second) == 0) queue.push_back(own->second);
    }
  }
  for (const std::string& key : all_members) {
    if (must_keep.count(key) > 0) {
      r.kept.insert(key);
      continue;
    }
    if (r.kept.count(key) == 0) r.dropped.push_back(key);
  }
  std::sort(r.dropped.begin(), r.dropped.end());
  return r;
}

}  // namespace

namespace {

bool is_file(const std::string& p) {
  struct stat st {};
  return ::stat(p.c_str(), &st) == 0 && !S_ISDIR(st.st_mode);
}

// find_lib 按 -L 顺序查找库文件，返回命中路径与**全部候选**。
// 两种命名都试（libfoo.hbc 与 foo.hbc）：前者是 unix 习惯，后者是我们自己
// `hermes build` 的默认产出名。没有 -L 时退化为当前目录。
std::string find_lib(const std::string& name, const std::vector<std::string>& dirs,
                     std::vector<std::string>* tried) {
  std::vector<std::string> ds = dirs;
  if (ds.empty()) ds.push_back(".");
  for (const std::string& d : ds) {
    const std::string prefix = (d == ".") ? std::string() : d + "/";
    for (const std::string& cand : {prefix + "lib" + name + ".hbc", prefix + name + ".hbc"}) {
      tried->push_back(cand);
      if (is_file(cand)) return cand;
    }
  }
  return std::string();
}

}  // namespace

// hm_tr 来自语言包（i18n_table.cpp）：key 就是英文原文。
extern "C" const char* hm_tr(const char* key, const char* lang);
static const char* tr(const char* key) {
  const char* env = std::getenv("HERMES_LANG");
  const char* lang = (env != nullptr && std::strncmp(env, "zh", 2) == 0) ? "zh" : "en";
  return hm_tr(key, lang);
}

int hm_link_main(int argc, char** argv) {
  std::string out_path;
  std::vector<std::string> inputs, search_dirs, lib_names;
  bool no_strip = false;
  for (int i = 2; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "--no-strip") {
      no_strip = true;
    } else if (a == "-o" || a == "--out") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes link: -o needs a file\n");
        return 2;
      }
      out_path = argv[++i];
    } else if (a == "-l") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes link: -l needs a library name\n");
        return 2;
      }
      lib_names.push_back(argv[++i]);
    } else if (a == "-L") {
      if (i + 1 >= argc) {
        std::fprintf(stderr, "hermes link: -L needs a directory\n");
        return 2;
      }
      search_dirs.push_back(argv[++i]);
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", tr("hermes link <a.hbc> [<b.hbc> ...] [-L <dir>] [-l <name>] [-o <out.hbc>] [--no-strip]"));
      return 0;
    } else if (!a.empty() && a[0] == '-') {
      std::fprintf(stderr, "hermes link: unknown argument %s\n", a.c_str());
      return 2;
    } else {
      inputs.push_back(a);
    }
  }
  // 解析 -l：只记名字，不在这里拼路径——`-L` 可能出现在 `-l` 之后，
  // 而参数顺序不该影响结果。
  for (const std::string& name : lib_names) {
    std::vector<std::string> tried;
    const std::string found = find_lib(name, search_dirs, &tried);
    if (found.empty()) {
      std::string joined;
      for (size_t i = 0; i < tried.size(); ++i) {
        if (i) joined += ", ";
        joined += tried[i];
      }
      std::fprintf(stderr, "hermes link: cannot find library \"%s\"; tried: %s\n", name.c_str(),
                   joined.c_str());
      return 4;
    }
    inputs.push_back(found);
  }
  if (inputs.empty()) {
    std::fprintf(stderr, "hermes link: missing input .hbc\n");
    return 2;
  }
  if (out_path.empty()) out_path = "a.out.hbc";

  std::vector<Input> ins;
  for (const std::string& p : inputs) {
    Input in;
    if (!load_input(p, &in)) {
      std::fprintf(stderr, "hermes link: cannot read %s\n", p.c_str());
      return 4;
    }
    ins.push_back(std::move(in));
  }

  // 先做依赖字节校验，再做符号解析：前者是"你拿的是不是你以为的那份"，
  // 后者是"这些份之间有没有冲突"。顺序反了会把投毒报成重名。
  for (const std::string& p : verify_requires(ins)) {
    std::fprintf(stderr, "hermes link: %s\n", p.c_str());
    return 4;
  }
  std::vector<std::string> problems;
  const LinkResult res = link_all(ins, &problems);
  if (!problems.empty()) {
    for (const std::string& p : problems) std::fprintf(stderr, "hermes link: %s\n", p.c_str());
    return 4;
  }

  hm::JVal members = merge_members(ins);
  if (!no_strip) {
    const ReachResult rr = reach(ins);
    if (!rr.problems.empty()) {
      for (const std::string& p : rr.problems) std::fprintf(stderr, "hermes link: %s\n", p.c_str());
      return 4;
    }
    if (rr.kept.empty()) {
      std::fprintf(stderr, "hermes link: nothing reachable after stripping (missing is_entry?)\n");
      return 4;
    }
    hm::JVal filtered = hm::JVal::array();
    for (size_t i = 0; i < members.size(); ++i) {
      const hm::JVal* m = members.at(i);
      if (m != nullptr && rr.kept.count(jstr(m, "name")) > 0) filtered.push(*m);
    }
    members = std::move(filtered);
    for (const std::string& d : rr.dropped) {
      std::fprintf(stderr, "hermes link: stripped %s (unreachable from entry)\n", d.c_str());
    }
  } else {
    std::fprintf(stderr, "hermes link: --no-strip: keeping all %d members\n",
                 static_cast<int>(members.size()));
  }

  const std::string mj = members.str();
  std::vector<unsigned char> out(1 << 20);
  size_t on = 0;
  int rc = hm_archive_write(mj.data(), mj.size(), out.data(), out.size(), &on);
  if (rc == -4 && on > out.size()) {
    out.resize(on);
    rc = hm_archive_write(mj.data(), mj.size(), out.data(), out.size(), &on);
  }
  if (rc != 0) {
    std::fprintf(stderr, "hermes link: cannot write archive (status %d)\n", rc);
    return 4;
  }
  FILE* f = std::fopen(out_path.c_str(), "wb");
  if (f == nullptr) {
    std::fprintf(stderr, "hermes link: cannot write %s\n", out_path.c_str());
    return 4;
  }
  std::fwrite(out.data(), 1, on, f);
  std::fclose(f);
  std::fprintf(stderr, "hermes link: wrote %s (%zu bytes, %d members, %d entries)\n",
               out_path.c_str(), on, static_cast<int>(res.members.size()),
               static_cast<int>(res.entries.size()));
  return 0;
}
