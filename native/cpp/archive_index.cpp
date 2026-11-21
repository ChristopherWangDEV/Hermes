// archive_index.cpp —— 从 `.hbc` 字节里取出**索引区**并算它的 sha256。
//
// 为什么这件事在 C++：`#link "lib"` 的 requires 需要 member_sha256，而那是
// **索引字节**的哈希（D-011：签名覆盖索引与成员哈希，不覆盖成员全文）。
// 迁移后装配在 C++，这个哈希就得能在 C++ 侧算出来，不能再绕回 Go。
//
// 无 IO：函数收**字节**，读文件是调用方的事（LANGUAGE §1.2 的同一原则）。
#include "hermes_parser.h"

#include <cstring>
#include <string>

extern "C" int hm_sha256(const unsigned char* data, size_t len, unsigned char* out);

namespace {

constexpr size_t kHeaderLen = 128;
constexpr size_t kFooterLen = 84;

uint16_t rd_u16(const unsigned char* p) { return static_cast<uint16_t>(p[0] | (p[1] << 8)); }
uint64_t rd_u64(const unsigned char* p) {
  uint64_t v = 0;
  for (int i = 7; i >= 0; --i) v = (v << 8) | p[i];
  return v;
}

}  // namespace

extern "C" int hm_archive_index_sha256(const unsigned char* data, size_t len, char* out_hex) {
  if (data == nullptr || out_hex == nullptr) return -1;
  if (len < kHeaderLen + kFooterLen) return 2;  // 太短：不是归档
  const char kMagic[4] = {'H', 'B', 'C', '\x1a'};
  if (std::memcmp(data, kMagic, 4) != 0) return 2;
  const uint16_t version = rd_u16(data + 4);
  if (version != 1) return 3;  // 版本不认识：拒绝比猜测安全

  const uint64_t index_off = rd_u64(data + 28);
  const uint64_t index_len = rd_u64(data + 36);
  // 边界：索引必须完整落在头部之后、footer 之前。越界一律报错，
  // 不能"截断着算一个哈希"——那会给出一个看起来正常但错误的指纹。
  if (index_off < kHeaderLen) return 4;
  if (index_len == 0) return 4;
  if (index_off > len || index_len > len - index_off) return 4;
  if (index_off + index_len > len - kFooterLen) return 4;

  unsigned char sum[32] = {0};
  if (hm_sha256(data + index_off, static_cast<size_t>(index_len), sum) != 0) return 5;
  static const char* kHex = "0123456789abcdef";
  for (int i = 0; i < 32; ++i) {
    out_hex[i * 2] = kHex[sum[i] >> 4];
    out_hex[i * 2 + 1] = kHex[sum[i] & 0xf];
  }
  out_hex[64] = '\0';
  return 0;
}
