// archive_write.cpp —— 写 `.hbc` 归档（IR.md §7 / D-013）。
//
// 布局（与 internal/archive 逐字节一致）：
//   [0,128)   头部：magic/version/成员数/三段偏移/索引 sha256
//   [128,..)  成员 IR 正文，按成员名**字典序**排列，每个 8 字节对齐
//   索引       canonical JSON（键按字节序）
//   footer 84 字节整段留 0（未签名）
//
// 为什么排序：链接顺序不该取决于调用方给的顺序，否则"同一批库、不同参数顺序"
// 会产出不同字节，可复现性立刻没了。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstring>
#include <string>
#include <vector>

extern "C" int hm_sha256(const unsigned char* data, size_t len, unsigned char* out);

namespace {

constexpr size_t kHeaderLen = 128;
constexpr size_t kFooterLen = 84;
constexpr size_t kAlign = 8;
constexpr int kVersion = 1;
const char* kMagic = "HBC\x1a";
const char* kCreatedBy = "hermes/1.0.0";

size_t align_up(size_t n) { return (n % kAlign == 0) ? n : n + (kAlign - n % kAlign); }

void put_u16(unsigned char* p, unsigned v) { p[0] = v & 0xff; p[1] = (v >> 8) & 0xff; }
void put_u32(unsigned char* p, unsigned v) {
  for (int i = 0; i < 4; ++i) p[i] = (v >> (8 * i)) & 0xff;
}
void put_u64(unsigned char* p, unsigned long long v) {
  for (int i = 0; i < 8; ++i) p[i] = (v >> (8 * i)) & 0xff;
}

std::string hex(const unsigned char* s, size_t n) {
  static const char* k = "0123456789abcdef";
  std::string o;
  for (size_t i = 0; i < n; ++i) {
    o += k[s[i] >> 4];
    o += k[s[i] & 0xf];
  }
  return o;
}

const char* str_or_empty(const hm::JVal* o, const char* k) {
  const hm::JVal* v = o->find(k);
  return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str().c_str() : "";
}

long long int_of(const hm::JVal* o, const char* k, long long dflt) {
  const hm::JVal* v = o->find(k);
  return (v != nullptr) ? v->as_int(dflt) : dflt;
}

}  // namespace

// members_json: [{"name":"m","ir":"<IR 文本>","abi_version":2,"language_version":2,
//                 "entry":false,
//                 "exports":{"workflows":[],"types":[],"backends":[],"tools":[]},
//                 "requires":[{"spec":..,"kind":..,"abi_version":..,"ir_version":..,
//                              "origin":..,"member_sha256":..,"implicit":false}]}]
extern "C" int hm_archive_write(const char* members_json, size_t len, unsigned char* out,
                                size_t cap, size_t* out_len) {
  if (members_json == nullptr || out == nullptr || out_len == nullptr) return -1;
  hm::JVal arr;
  size_t pos = 0;
  if (!hm::parse_json(members_json, len, &arr, &pos) || arr.kind() != hm::JVal::kArr) return -1;
  if (arr.size() == 0) return -2;  // 空归档必须显式拒绝

  // 按成员名排序（§9.4）。
  std::vector<const hm::JVal*> ms;
  for (size_t i = 0; i < arr.size(); ++i) {
    const hm::JVal* m = arr.at(i);
    if (m != nullptr && m->kind() == hm::JVal::kObj) ms.push_back(m);
  }
  std::sort(ms.begin(), ms.end(), [](const hm::JVal* a, const hm::JVal* b) {
    return std::strcmp(str_or_empty(a, "name"), str_or_empty(b, "name")) < 0;
  });

  // 成员正文 + 各自的 ir_sha256。
  std::vector<std::string> bodies;
  std::vector<std::string> sums;
  size_t off = kHeaderLen;
  for (const hm::JVal* m : ms) {
    const std::string body = str_or_empty(m, "ir");
    unsigned char sum[32] = {0};
    if (hm_sha256(reinterpret_cast<const unsigned char*>(body.data()), body.size(), sum) != 0) {
      return -3;
    }
    bodies.push_back(body);
    sums.push_back(hex(sum, 32));
    off = align_up(off + body.size());
  }
  const size_t index_off = off;

  // 索引（canonical JSON：键排序由 JVal::str 保证）。
  hm::JVal members = hm::JVal::array();
  hm::JVal symbols = hm::JVal::array();
  hm::JVal entries = hm::JVal::array();
  size_t cur = kHeaderLen;
  for (size_t i = 0; i < ms.size(); ++i) {
    const hm::JVal* m = ms[i];
    const std::string name = str_or_empty(m, "name");

    hm::JVal rqs = hm::JVal::array();
    const hm::JVal* rq = m->find("requires");
    if (rq != nullptr && rq->kind() == hm::JVal::kArr) {
      for (size_t j = 0; j < rq->size(); ++j) {
        const hm::JVal* r = rq->at(j);
        if (r == nullptr) continue;
        hm::JVal o = hm::JVal::object();
        o.set("spec", hm::JVal::string(str_or_empty(r, "spec")));
        o.set("kind", hm::JVal::string(str_or_empty(r, "kind")));
        o.set("abi_version", hm::JVal::integer(int_of(r, "abi_version", 0)));
        o.set("ir_version", hm::JVal::integer(int_of(r, "ir_version", 0)));
        o.set("origin", hm::JVal::string(str_or_empty(r, "origin")));
        o.set("member_sha256", hm::JVal::string(str_or_empty(r, "member_sha256")));
        const hm::JVal* imp = r->find("implicit");
        if (imp != nullptr && imp->kind() == hm::JVal::kBool && imp->as_bool(false)) {
          o.set("implicit", hm::JVal::boolean(true));
        }
        rqs.push(std::move(o));
      }
    }

    const hm::JVal* ex = m->find("exports");
    auto export_arr = [&](const char* key) {
      hm::JVal a = hm::JVal::array();
      const hm::JVal* src = (ex != nullptr) ? ex->find(key) : nullptr;
      if (src != nullptr && src->kind() == hm::JVal::kArr) {
        for (size_t j = 0; j < src->size(); ++j) {
          const hm::JVal* v = src->at(j);
          if (v != nullptr && v->kind() == hm::JVal::kStr) a.push(hm::JVal::string(v->as_str()));
        }
      }
      return a;
    };

    hm::JVal mo = hm::JVal::object();
    mo.set("name", hm::JVal::string(name));
    mo.set("ir_off", hm::JVal::integer(static_cast<long long>(cur)));
    mo.set("ir_len", hm::JVal::integer(static_cast<long long>(bodies[i].size())));
    mo.set("ir_sha256", hm::JVal::string(sums[i]));
    mo.set("abi_version", hm::JVal::integer(int_of(m, "abi_version", 0)));
    mo.set("ir_version", hm::JVal::integer(1));
    mo.set("language_version", hm::JVal::integer(int_of(m, "language_version", 0)));
    hm::JVal eo = hm::JVal::object();
    eo.set("workflows", export_arr("workflows"));
    eo.set("types", export_arr("types"));
    eo.set("backends", export_arr("backends"));
    eo.set("tools", export_arr("tools"));
    mo.set("exports", std::move(eo));
    mo.set("requires", std::move(rqs));
    members.push(std::move(mo));

    const hm::JVal* ent = m->find("entry");
    const bool is_entry = (ent != nullptr && ent->kind() == hm::JVal::kBool && ent->as_bool(false));
    const hm::JVal* wfs = (ex != nullptr) ? ex->find("workflows") : nullptr;
    if (wfs != nullptr && wfs->kind() == hm::JVal::kArr) {
      for (size_t j = 0; j < wfs->size(); ++j) {
        const hm::JVal* w = wfs->at(j);
        if (w == nullptr || w->kind() != hm::JVal::kStr) continue;
        hm::JVal s = hm::JVal::object();
        s.set("member", hm::JVal::string(name));
        s.set("kind", hm::JVal::string("workflow"));
        s.set("name", hm::JVal::string(w->as_str()));
        symbols.push(std::move(s));
        if (is_entry) entries.push(hm::JVal::string(w->as_str()));
      }
    }
    const hm::JVal* tys = (ex != nullptr) ? ex->find("types") : nullptr;
    if (tys != nullptr && tys->kind() == hm::JVal::kArr) {
      for (size_t j = 0; j < tys->size(); ++j) {
        const hm::JVal* t = tys->at(j);
        if (t == nullptr || t->kind() != hm::JVal::kStr) continue;
        hm::JVal s = hm::JVal::object();
        s.set("member", hm::JVal::string(name));
        s.set("kind", hm::JVal::string("type"));
        s.set("name", hm::JVal::string(t->as_str()));
        symbols.push(std::move(s));
      }
    }
    cur = align_up(cur + bodies[i].size());
  }

  hm::JVal root = hm::JVal::object();
  root.set("archive_version", hm::JVal::integer(kVersion));
  root.set("created_by", hm::JVal::string(kCreatedBy));
  root.set("flags", hm::JVal::integer(0));
  root.set("members", std::move(members));
  root.set("symbols", std::move(symbols));
  root.set("entries", std::move(entries));
  const std::string index = root.str();

  const size_t file_len = index_off + index.size() + kFooterLen;
  if (file_len > cap) {
    *out_len = file_len;
    return -4;
  }
  std::memset(out, 0, file_len);
  std::memcpy(out, kMagic, 4);
  put_u16(out + 4, kVersion);
  put_u32(out + 8, static_cast<unsigned>(ms.size()));
  put_u64(out + 12, kHeaderLen);
  put_u64(out + 20, static_cast<unsigned long long>(index_off - kHeaderLen));
  put_u64(out + 28, static_cast<unsigned long long>(index_off));
  put_u64(out + 36, static_cast<unsigned long long>(index.size()));
  {
    unsigned char sum[32] = {0};
    if (hm_sha256(reinterpret_cast<const unsigned char*>(index.data()), index.size(), sum) != 0) {
      return -3;
    }
    std::memcpy(out + 44, sum, 32);
  }
  cur = kHeaderLen;
  for (size_t i = 0; i < bodies.size(); ++i) {
    std::memcpy(out + cur, bodies[i].data(), bodies[i].size());
    cur = align_up(cur + bodies[i].size());
  }
  std::memcpy(out + index_off, index.data(), index.size());
  *out_len = file_len;
  return 0;
}
