// trace_cmd.cpp —— `hermes trace verify|summary|diff`：单文件回答"哪步失败、
// 花多少、调了几次"（R-TR-05：不触网、不读源码）。
//
// `verify` 是**安全语义**：把被篡改的 trace 判成干净，比不检查更糟。所以它的
// 判据是 tests/trace/golden.json 里冻结的 8 个用例（在 Go 实现还在时录的）。
//
// 链的哈希由 C ABI 的 hm_chain_hash / hm_chain_final 算（与 writer 同一份实现），
// 本文件只负责**规范化每个事件**并串起来——规范化用 JVal 的 canonical 写出器，
// 实测能逐字节复现 writer 的 block_hashes 与 root。
#include "hermes_parser.h"
#include "json_write.h"

#include <algorithm>
#include <cstdio>
#include <cstring>
#include <map>
#include <string>
#include <vector>

extern "C" int hm_chain_hash(const unsigned char* prev, unsigned long long index,
                             const unsigned char* block, size_t n, unsigned char* out);
extern "C" int hm_chain_final(const unsigned char* prev, unsigned long long count,
                              unsigned long long total, unsigned char* out);

namespace {

std::string hex32(const unsigned char* p) {
  static const char* H = "0123456789abcdef";
  std::string s;
  s.reserve(64);
  for (int i = 0; i < 32; ++i) {
    s.push_back(H[p[i] >> 4]);
    s.push_back(H[p[i] & 0xf]);
  }
  return s;
}

bool read_all(const std::string& path, std::string* out) {
  FILE* f = std::fopen(path.c_str(), "rb");
  if (f == nullptr) return false;
  char buf[16384];
  size_t n;
  out->clear();
  while ((n = std::fread(buf, 1, sizeof buf, f)) > 0) out->append(buf, n);
  std::fclose(f);
  return true;
}

std::string jstr(const hm::JVal* o, const char* k) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return (v != nullptr && v->kind() == hm::JVal::kStr) ? v->as_str() : "";
}
long long jint(const hm::JVal* o, const char* k) {
  const hm::JVal* v = (o != nullptr) ? o->find(k) : nullptr;
  return (v != nullptr && v->kind() == hm::JVal::kInt) ? v->as_int(0) : 0;
}

struct ChainInfo {
  std::string root;
  size_t blocks = 0;
  long long total_len = 0;
  bool truncated = false;
  std::vector<std::string> block_hashes;
};

// verify_chain 重算链，返回是否通过；失败时填 block/want/got。
bool verify_chain(const std::string& data, ChainInfo* info, size_t* bad_block, std::string* want,
                  std::string* got, std::string* detail) {
  hm::JVal top;
  size_t pos = 0;
  if (!hm::parse_json(data.data(), data.size(), &top, &pos) || top.kind() != hm::JVal::kObj) {
    *detail = "trace: parse failed";
    return false;
  }
  const hm::JVal* events = top.find("events");
  const hm::JVal* chain = top.find("chain");
  if (events == nullptr || events->kind() != hm::JVal::kArr) {
    *detail = "trace: missing events";
    return false;
  }
  if (chain == nullptr || chain->kind() != hm::JVal::kObj) {
    *detail = "trace: missing chain";
    return false;
  }
  unsigned char prev[32] = {0};
  long long total = 0;
  std::vector<std::string> hashes;
  for (size_t i = 0; i < events->size(); ++i) {
    const hm::JVal* e = events->at(i);
    if (e == nullptr) continue;
    const std::string body = e->str();
    unsigned char out[32] = {0};
    if (hm_chain_hash(prev, i, reinterpret_cast<const unsigned char*>(body.data()), body.size(),
                      out) != 0) {
      *detail = "trace: chain hash failed";
      return false;
    }
    std::memcpy(prev, out, 32);
    hashes.push_back(hex32(out));
    total += static_cast<long long>(body.size());
  }
  unsigned char root[32] = {0};
  if (hm_chain_final(prev, events->size(), static_cast<unsigned long long>(total), root) != 0) {
    *detail = "trace: chain final failed";
    return false;
  }
  info->root = hex32(root);
  info->blocks = events->size();
  info->total_len = total;
  info->truncated = false;
  info->block_hashes = hashes;
  {
    const hm::JVal* tr = chain->find("truncated");
    if (tr != nullptr && tr->kind() == hm::JVal::kBool) info->truncated = tr->as_bool(false);
  }

  const std::string got_root = jstr(chain, "root");
  if (got_root == info->root) return true;

  // 根对不上 → 逐块定位。文件里存了 block_hashes，才能回答"哪一块"。
  const hm::JVal* want_h = chain->find("block_hashes");
  if (want_h != nullptr && want_h->kind() == hm::JVal::kArr && want_h->size() == hashes.size()) {
    for (size_t i = 0; i < hashes.size(); ++i) {
      const hm::JVal* w = want_h->at(i);
      const std::string ws = (w != nullptr && w->kind() == hm::JVal::kStr) ? w->as_str() : "";
      if (ws != hashes[i]) {
        *bad_block = i;
        *want = ws;
        *got = hashes[i];
        return false;
      }
    }
  }
  *bad_block = 0;
  *want = got_root;
  *got = info->root;
  return false;
}

}  // namespace

namespace {

struct SpanRow {
  std::string step_id, span_id, status, attr, kind;
  long long micro_usd = 0, tokens_in = 0, tokens_out = 0;
};

std::vector<SpanRow> parse_spans(const std::string& data, bool* ok) {
  std::vector<SpanRow> out;
  hm::JVal top;
  size_t pos = 0;
  if (!hm::parse_json(data.data(), data.size(), &top, &pos) || top.kind() != hm::JVal::kObj) {
    *ok = false;
    return out;
  }
  const hm::JVal* evs = top.find("events");
  if (evs == nullptr || evs->kind() != hm::JVal::kArr) {
    *ok = false;
    return out;
  }
  *ok = true;
  for (size_t i = 0; i < evs->size(); ++i) {
    const hm::JVal* e = evs->at(i);
    if (e == nullptr || e->kind() != hm::JVal::kObj) continue;
    SpanRow s;
    s.step_id = jstr(e, "step_id");
    s.span_id = jstr(e, "span_id");
    s.status = jstr(e, "status");
    s.attr = jstr(e, "attr");
    s.kind = jstr(e, "kind");
    s.micro_usd = jint(e, "micro_usd");
    s.tokens_in = jint(e, "tokens_in");
    s.tokens_out = jint(e, "tokens_out");
    out.push_back(std::move(s));
  }
  return out;
}

std::string json_escape(const std::string& s) {
  std::string o;
  for (char c : s) {
    switch (c) {
      case '"': o += "\\\""; break;
      case '\\': o += "\\\\"; break;
      case '\n': o += "\\n"; break;
      case '\r': o += "\\r"; break;
      case '\t': o += "\\t"; break;
      default:
        if (static_cast<unsigned char>(c) < 0x20) {
          char b[8];
          std::snprintf(b, sizeof b, "\\u%04x", c);
          o += b;
        } else {
          o.push_back(c);
        }
    }
  }
  return o;
}

// 语言包：key 就是英文原文（与其它 C++ 侧同一约定）。
extern "C" const char* hm_tr(const char* key, const char* lang);
static const char* tr(const char* key) {
  const char* env = std::getenv("HERMES_LANG");
  const char* lang = (env != nullptr && std::strncmp(env, "zh", 2) == 0) ? "zh" : "en";
  return hm_tr(key, lang);
}

const char* trace_usage() {
  return tr("hermes trace: verify <trace.json> | summary <trace.json> | diff <a.json> <b.json> [--json]");
}

}  // namespace

int hm_trace_main(int argc, char** argv) {
  if (argc < 3) {
    std::fprintf(stderr, "%s\n", trace_usage());
    return 2;
  }
  const std::string sub = argv[2];
  if (sub == "-h" || sub == "--help" || sub == "help") {
    std::printf("%s\n", trace_usage());
    return 0;
  }
  std::vector<std::string> pos;
  bool as_json = false;
  for (int i = 3; i < argc; ++i) {
    const std::string a = argv[i];
    if (a == "--json" || a == "-json") {
      as_json = true;
    } else if (a == "-h" || a == "--help" || a == "help") {
      std::printf("%s\n", trace_usage());
      return 0;
    } else {
      pos.push_back(a);
    }
  }

  if (sub == "verify") {
    if (pos.size() != 1) {
      std::fprintf(stderr, "hermes trace verify: needs exactly one file path\n");
      return 2;
    }
    std::string data;
    if (!read_all(pos[0], &data)) {
      std::fprintf(stderr, "hermes trace: cannot read %s\n", pos[0].c_str());
      return 4;
    }
    ChainInfo info;
    size_t bad = 0;
    std::string want, got, detail;
    if (!verify_chain(data, &info, &bad, &want, &got, &detail)) {
      if (!detail.empty()) {
        std::fprintf(stderr, "hermes trace: %s\n", detail.c_str());
        return 4;
      }
      // 篡改是**结论**，不是内部错误：退出码 4，并明确指出第几块。
      std::fprintf(stderr, "FAIL: trace tampered at block %zu (want %s, got %s)\n", bad,
                   want.c_str(), got.c_str());
      if (as_json) {
        // Go 的 map 编码按字典序：block, detail, got, verified, want。
        std::printf("{\"block\":%zu,\"detail\":\"\",\"got\":\"%s\",\"verified\":false,\"want\":\"%s\"}\n",
                    bad, got.c_str(), want.c_str());
      }
      return 4;
    }
    if (as_json) {
      std::printf(
          "{\"blocks\":%zu,\"root\":\"%s\",\"total_len\":%lld,\"truncated\":%s,\"verified\":true}\n",
          info.blocks, info.root.c_str(), info.total_len,
          info.truncated ? "true" : "false");
      return 0;
    }
    std::printf("OK\n");
    std::printf("  root      %s\n", info.root.c_str());
    std::printf("  blocks    %zu\n", info.blocks);
    std::printf("  total_len %lld\n", info.total_len);
    if (info.truncated) {
      std::printf("  %s\n", tr("truncated       true (this trace was cut by max_bytes: incomplete, but not tampered with)"));
    }
    return 0;
  }

  if (sub == "summary") {
    if (pos.size() != 1) {
      std::fprintf(stderr, "hermes trace summary: needs exactly one file path\n");
      return 2;
    }
    std::string data;
    if (!read_all(pos[0], &data)) {
      std::fprintf(stderr, "hermes trace: cannot read %s\n", pos[0].c_str());
      return 4;
    }
    bool ok = false;
    const std::vector<SpanRow> spans = parse_spans(data, &ok);
    if (!ok) {
      std::fprintf(stderr, "hermes trace: trace: parse failed\n");
      return 4;
    }
    hm::JVal top;
    size_t tp = 0;
    hm::parse_json(data.data(), data.size(), &top, &tp);
    const std::string run_id = jstr(&top, "run_id");

    std::string failed_step, failed_span, attribution;
    long long micro = 0, tin = 0, tout = 0;
    int calls = 0, tools = 0;
    bool truncated = false;
    for (const SpanRow& s : spans) {
      micro += s.micro_usd;
      tin += s.tokens_in;
      tout += s.tokens_out;
      if (s.kind == "provider") ++calls;
      if (s.kind == "tool") ++tools;
      if (s.status == "truncated") truncated = true;
      if (failed_step.empty() && s.status == "failed") {
        failed_step = s.step_id;
        failed_span = s.span_id;
        attribution = s.attr;
      }
    }
    if (as_json) {
      // 结构体字段顺序（不是字典序）：run_id, spans, failed_step, failed_span,
      // attribution, micro_usd, tokens_in, tokens_out, provider_calls, tool_calls, truncated。
      std::printf(
          "{\"run_id\":\"%s\",\"spans\":%zu,\"failed_step\":\"%s\",\"failed_span\":\"%s\","
          "\"attribution\":\"%s\",\"micro_usd\":%lld,\"tokens_in\":%lld,\"tokens_out\":%lld,"
          "\"provider_calls\":%d,\"tool_calls\":%d,\"truncated\":%s}\n",
          json_escape(run_id).c_str(), spans.size(), json_escape(failed_step).c_str(),
          json_escape(failed_span).c_str(), json_escape(attribution).c_str(), micro, tin, tout,
          calls, tools, truncated ? "true" : "false");
      return 0;
    }
    std::printf("run        %s\n", run_id.c_str());
    std::printf("spans      %zu\n", spans.size());
    if (!failed_step.empty()) {
      std::printf("failed_at  %s (span %s)\n", failed_step.c_str(), failed_span.c_str());
      std::printf("attribution %s\n", attribution.c_str());
    } else {
      std::printf("%s\n", tr("failed_at  - (no failed step)"));
    }
    std::printf("calls      provider=%d tool=%d\n", calls, tools);
    std::printf("tokens     in=%lld out=%lld\n", tin, tout);
    std::printf("cost       %lld micro-USD\n", micro);
    if (truncated) std::printf("truncated  true\n");
    return 0;
  }

  if (sub == "diff") {
    if (pos.size() != 2) {
      std::fprintf(stderr, "hermes trace diff: needs exactly two trace files\n");
      return 2;
    }
    bool ok1 = false, ok2 = false;
    std::string d1, d2;
    if (!read_all(pos[0], &d1) || !read_all(pos[1], &d2)) {
      std::fprintf(stderr, "hermes trace: cannot read trace file\n");
      return 4;
    }
    const std::vector<SpanRow> l = parse_spans(d1, &ok1);
    const std::vector<SpanRow> r = parse_spans(d2, &ok2);
    if (!ok1 || !ok2) {
      std::fprintf(stderr, "hermes trace diff: cannot parse trace\n");
      return 4;
    }
    // 按 step_id 对齐；同一个 step 有多条 span（重试）时**保留第一条失败的**。
    auto by_step = [](const std::vector<SpanRow>& spans) {
      std::map<std::string, SpanRow> m;
      for (const SpanRow& s : spans) {
        auto it = m.find(s.step_id);
        if (it != m.end()) {
          if (it->second.status == "failed" || s.status != "failed") continue;
        }
        m[s.step_id] = s;
      }
      return m;
    };
    const auto lm = by_step(l), rm = by_step(r);
    std::vector<std::string> only_left, only_right;
    std::vector<std::string> changed;
    int failed_left = 0, failed_right = 0;
    for (const auto& kv : lm) {
      auto it = rm.find(kv.first);
      if (it != rm.end()) {
        if (kv.second.status != it->second.status) {
          changed.push_back("~ " + kv.first + ": " + kv.second.status + " -> " + it->second.status);
        }
      } else {
        only_left.push_back(kv.first);
      }
      if (kv.second.status == "failed") ++failed_left;
    }
    for (const auto& kv : rm) {
      if (lm.count(kv.first) == 0) only_right.push_back(kv.first);
      if (kv.second.status == "failed") ++failed_right;
    }
    std::sort(only_left.begin(), only_left.end());
    std::sort(only_right.begin(), only_right.end());
    std::sort(changed.begin(), changed.end());
    long long token_delta = 0, cost_delta = 0;
    for (const SpanRow& s : l) {
      token_delta -= s.tokens_in + s.tokens_out;
      cost_delta -= s.micro_usd;
    }
    for (const SpanRow& s : r) {
      token_delta += s.tokens_in + s.tokens_out;
      cost_delta += s.micro_usd;
    }
    if (as_json) {
      // MarshalIndent 结构体顺序：OnlyLeft, OnlyRight, Changed, TokenDelta,
      // CostDelta, FailedLeft, FailedRight。空切片编码成 null。
      auto arr = [](const std::vector<std::string>& v) {
        if (v.empty()) return std::string("null");
        std::string s = "[";
        for (size_t i = 0; i < v.size(); ++i) {
          if (i) s += ",";
          s += "\"" + json_escape(v[i]) + "\"";
        }
        return s + "]";
      };
      std::printf("{\n  \"OnlyLeft\": %s,\n  \"OnlyRight\": %s,\n  \"Changed\": %s,\n",
                  arr(only_left).c_str(), arr(only_right).c_str(), arr(changed).c_str());
      std::printf("  \"TokenDelta\": %lld,\n  \"CostDelta\": %lld,\n  \"FailedLeft\": %d,\n  \"FailedRight\": %d\n}\n",
                  token_delta, cost_delta, failed_left, failed_right);
      return 0;
    }
    for (const std::string& id : only_left) std::printf("- %s\n", id.c_str());
    for (const std::string& id : only_right) std::printf("+ %s\n", id.c_str());
    for (const std::string& c : changed) std::printf("%s\n", c.c_str());
    std::printf("tokens %+lld, cost micro_usd %+lld, failed %d -> %d\n", token_delta, cost_delta,
                failed_left, failed_right);
    return 0;
  }

  std::fprintf(stderr, "hermes trace: unknown subcommand \"%s\"\n", sub.c_str());
  return 2;
}
