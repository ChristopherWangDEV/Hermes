// json_write.h —— canonical JSON 写出器（C++ 侧）。
//
// 为什么不用第三方库：红线（零第三方 C++ 库）+ 我们只需要一个方向和一种格式。
// 为什么手写而不是"拼字符串"：键序、转义、整数范围这三件事一旦靠调用方自觉，
// 迟早会出现"看起来一样但字节不同"的两个产物，而 IR 的全部价值建立在字节稳定上。
//
// canonical 的三条（与 internal/canon 一致，两边由 golden 对齐）：
//   1. 对象的键按**字节序**排列（不是插入序）；
//   2. 没有多余空白（无换行、无缩进、`:` 与 `,` 后都不加空格）；
//   3. **不出现浮点 JSON 数字**。canonical / 字节稳定 / 可签名都建立在"同一个值
//      只有一种写法"上，而浮点的最短往返表示在 Go/Rust/C++ 之间有真实分歧。
//      浮点**常量**因此编码成位模式对象：
//        {"k":"double","bits":"3ff0000000000000"}
//      —— 两个字段都是 ASCII 字符串，bits 是 16 位小写 hex（IEEE-754 binary64
//      的位模式用 %016x 格式化），**不是 JSON 数字**。JVal::Kind 不需要新增
//      枚举值：写出去的就是一个 object 里塞两个 string。
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <utility>
#include <vector>

namespace hm {

// JVal 是一棵 JSON 值树。
//
// 用树而不是流式写：canonical 要求键排序，而排序需要先知道全部键。
// 流式写只能在调用点手工保证顺序 —— 那正是"同一份数据两种字节"的来源。
class JVal {
 public:
  enum Kind { kNull, kBool, kInt, kStr, kArr, kObj };

  JVal() : k_(kNull), b_(false), i_(0) {}

  static JVal null() { return JVal(); }
  static JVal boolean(bool b);
  static JVal integer(std::int64_t i);
  static JVal string(const std::string& s);
  static JVal string(const char* s);
  static JVal array();
  static JVal object();

  // push 往数组尾部追加；set 往对象里插入或覆盖同名的键。
  // 对非数组/非对象调用是**未定义行为**，但调用方是我们自己的 codegen，
  // 而且下面写了断言式的兜底（kNull 会被忽略而不是静默写入错误结构）。
  JVal& push(JVal v);
  JVal& set(const std::string& key, JVal v);

  Kind kind() const { return k_; }
  bool empty_obj() const { return k_ == kObj && o_.empty(); }

  // 序列化进调用方缓冲。返回**需要**的字节数（含结尾不会写 '\0'）：
  // 返回值 > cap 表示被截断，调用方应按返回值扩容重试 —— 与 hm_parse_* 的
  // 两段式约定一致，不另造一套。
  std::size_t write(char* out, std::size_t cap) const;

  // 便捷版：只在测试与调试里用（会分配）。
  std::string str() const;

 private:
  void append(std::string& b) const;

  Kind k_;
  bool b_;
  std::int64_t i_;
  std::string s_;
  std::vector<JVal> a_;
  std::vector<std::pair<std::string, JVal>> o_;
};

// 转义一个字符串、按 JSON 规则写进 out（不含外层引号由调用方负责）。
// 只对 < 0x20 的字符做 \u00XX，其余原样输出 UTF-8 —— 转义范围比需要的更大
// 会让同一份中文文本出现两种合法写法，而 canonical 只允许一种。
void write_escaped(std::string& out, const std::string& s);

}  // namespace hm
