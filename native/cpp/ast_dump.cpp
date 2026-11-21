// ast_dump.cpp —— 确定性 AST 序列化（给人读、给 golden 比对的文本）。
//
// 约束：无 IO、无异常、无 RTTI。输出写入调用方提供的字节缓冲。
// 遍历顺序固定（child → type_args → args → else_branch → subs），不依赖指针地址，
// 缩进只由深度决定 —— 因此同一 AST 永远得到同一份文本，diff 有意义。

#include "ast.h"

#include <cstdarg>
#include <cstdio>
#include <cstring>

namespace hm {

namespace {

struct Buf {
  char* o;
  char* end;
  bool overflow;
};

void put_raw(Buf& b, const char* s) {
  while (*s) {
    if (b.o + 1 >= b.end) { b.overflow = true; return; }
    *b.o++ = *s++;
  }
}

// 某个节点的直接子节点遍历（固定顺序）。
template <class F>
void for_each_child(const Node* n, F f) {
  for (Node* c = n->child; c; c = c->next) f(c);
  for (Node* c = n->type_args; c; c = c->next) f(c);
  for (Node* c = n->args; c; c = c->next) f(c);
  if (n->else_branch) f(n->else_branch);
  for (int k = 0; k < n->n_subs; ++k) f(&n->subs[k]);
}

int count_children(const Node* n) {
  int c = 0;
  for_each_child(n, [&](const Node*) { c++; });
  return c;
}

void dump_node(const Node* n, int depth, Buf& b, int* count, int* depth_guard) {
  if (!n || *depth_guard <= 0) { b.overflow = true; return; }
  (*depth_guard)--;
  (*count)++;
  char tmp[256];
  int nch = count_children(n);
  const char* t = n->text ? n->text : "";
  snprintf(tmp, sizeof(tmp), "%*s%s n=%d %d:%d-%d:%d \"%s\"\n",
           depth * 2, "", kind_name(n->kind), nch,
           n->loc.line, n->loc.col, n->loc.end_line, n->loc.end_col, t);
  put_raw(b, tmp);
  for_each_child(n, [&](const Node* c) { dump_node(c, depth + 1, b, count, depth_guard); });
  (*depth_guard)++;
}

}  // namespace

// 返回 AST 节点总数（用于 golden 的"节点数 == K"断言）。
int ast_count(const Node* root) {
  if (!root) return 0;
  int c = 0;
  // 仅计数：复用 dump 的遍历但只累加。
  // 简单起见直接走一遍 dump（到 /dev/null 风格缓冲），再数行。
  char sink[8];
  Buf b{sink, sink + sizeof(sink), false};
  int depth_guard = 100000;
  dump_node(root, 0, b, &c, &depth_guard);
  return c;
}

// 把 AST 序列化为确定性文本，写入 out（容量 cap）。*out_len 含结尾 '\0'。
void ast_dump(const Node* root, char* out, size_t cap, size_t* out_len) {
  if (cap == 0) { if (out_len) *out_len = 0; return; }
  Buf b{out, out + cap, false};
  int count = 0;
  int depth_guard = 100000;
  dump_node(root, 0, b, &count, &depth_guard);
  if (b.o < b.end) *b.o = '\0'; else b.o[-1] = '\0';
  if (out_len) *out_len = static_cast<size_t>(b.o - out) + 1;
}

}  // namespace hm
