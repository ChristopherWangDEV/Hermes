// json_write_reader.cpp —— JVal 的只读访问器。
//
// 与 json_write.cpp 分开文件：那个文件只管"写出的字节必须 canonical"，
// 这个只管"读进来怎么问值"。两件事的失败模式完全不同（写错是产物漂移，
// 读错是索引错位），混在一起会让 review 时看不出哪条规则在管哪个方向。
#include "json_write.h"

namespace hm {

const JVal* JVal::find(const std::string& key) const {
  if (k_ != kObj) return nullptr;
  for (const auto& kv : o_) {
    if (kv.first == key) return &kv.second;
  }
  return nullptr;
}

const JVal* JVal::at(std::size_t idx) const {
  if (k_ != kArr || idx >= a_.size()) return nullptr;
  return &a_[idx];
}

std::size_t JVal::size() const {
  if (k_ == kArr) return a_.size();
  if (k_ == kObj) return o_.size();
  return 0;
}

const std::string& JVal::as_str() const {
  static const std::string kEmpty;
  return k_ == kStr ? s_ : kEmpty;
}

void JVal::keys(std::vector<std::string>* out) const {
  if (out == nullptr || k_ != kObj) return;
  for (const auto& kv : o_) out->push_back(kv.first);
}

}  // namespace hm
