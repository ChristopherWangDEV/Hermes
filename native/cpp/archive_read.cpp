// archive_read.cpp —— 读 `.hbc`：校验结构 + 取出索引里的成员描述。
//
// 无 IO：收字节（读文件是调用方的事）。校验顺序与 `internal/archive.Read` 一致：
//   魔数 → 版本 → 长度 → 索引 sha256 → 索引 JSON 可解析。
// **先验哈希再解析 JSON**：否则一份被篡改的归档可以让我们去解析任意字节。
#include "hermes_parser.h"
#include "json_write.h"

#include <cstring>
#include <string>

extern "C" int hm_sha256(const unsigned char* data, size_t len, unsigned char* out);

namespace {

constexpr size_t kHeaderLen = 128;
constexpr size_t kFooterLen = 84;

unsigned rd_u16(const unsigned char* p) { return p[0] | (p[1] << 8); }
unsigned long long rd_u64(const unsigned char* p) {
  unsigned long long v = 0;
  for (int i = 7; i >= 0; --i) v = (v << 8) | p[i];
  return v;
}

}  // namespace

// 成功返回 0，out 是成员描述的 JSON 数组（索引里的 members，原样透出）；
// 2 不是归档；3 版本不认识；4 结构/哈希/JSON 有问题。
extern "C" int hm_archive_read(const unsigned char* data, size_t len, char* out, size_t cap,
                               size_t* out_len) {
  if (data == nullptr || out == nullptr || out_len == nullptr) return -1;
  if (len < kHeaderLen + kFooterLen) return 2;
  const char kMagic[4] = {'H', 'B', 'C', '\x1a'};
  if (std::memcmp(data, kMagic, 4) != 0) return 2;
  if (rd_u16(data + 4) != 1) return 3;

  const unsigned long long index_off = rd_u64(data + 28);
  const unsigned long long index_len = rd_u64(data + 36);
  if (index_off < kHeaderLen) return 4;
  if (index_len == 0 || index_off > len || index_len > len - index_off) return 4;
  if (index_off + index_len > len - kFooterLen) return 4;

  // 索引自身的 sha256 必须与头部记录一致：不一致说明归档被改过。
  unsigned char sum[32] = {0};
  if (hm_sha256(data + index_off, static_cast<size_t>(index_len), sum) != 0) return 4;
  if (std::memcmp(sum, data + 44, 32) != 0) return 4;

  hm::JVal root;
  size_t pos = 0;
  if (!hm::parse_json(reinterpret_cast<const char*>(data + index_off),
                      static_cast<size_t>(index_len), &root, &pos) ||
      root.kind() != hm::JVal::kObj) {
    return 4;
  }
  const hm::JVal* members = root.find("members");
  if (members == nullptr || members->kind() != hm::JVal::kArr) return 4;

  // 每个成员的正文区间也验一遍：偏移/长度必须落在"头部之后、索引之前"。
  for (size_t i = 0; i < members->size(); ++i) {
    const hm::JVal* m = members->at(i);
    if (m == nullptr || m->kind() != hm::JVal::kObj) return 4;
    const hm::JVal* off = m->find("ir_off");
    const hm::JVal* n = m->find("ir_len");
    if (off == nullptr || n == nullptr) return 4;
    const unsigned long long o = static_cast<unsigned long long>(off->as_int(0));
    const unsigned long long k = static_cast<unsigned long long>(n->as_int(0));
    if (o < kHeaderLen || o > index_off || k > index_off - o) return 4;
  }

  const std::string js = members->str();
  const size_t need = js.size() + 1;
  if (need > cap) {
    *out_len = need;
    return -4;
  }
  std::memcpy(out, js.data(), js.size());
  out[js.size()] = 0;
  *out_len = need;
  return 0;
}
