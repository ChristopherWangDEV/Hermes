// linkpolicy_table.h —— 由 tools/genlinkpolicy 生成的定义的类型（手写，只为声明）。
#include <cstddef>

namespace hm {
struct LinkRule {
  const char* name;
  const char* licence;
  const char* reason;
  const char* source;
};
extern const LinkRule kForbiddenRules[];
extern const size_t kForbiddenRuleCount;
}  // namespace hm
