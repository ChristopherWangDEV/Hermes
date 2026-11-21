// std_variants.h —— 由 tools/genstdvariants 生成的数据的类型（手写，只声明）。
#include <cstddef>

namespace hm {
struct StdVariantRow {
  const char* mod;
  const char* variant;
  const char* name;
  bool go_impl;
};
extern const StdVariantRow kStdVariants[];
extern const size_t kStdVariantCount;
}  // namespace hm
