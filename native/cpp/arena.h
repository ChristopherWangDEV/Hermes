// arena.h —— Hermes 编译器前端的 AST 内存池（bump allocator）。
//
// 设计约束（ARCH §5.9 / LANGUAGE 前端硬约束）：
//   · -fno-exceptions：分配失败**绝不抛异常**，只返回 nullptr + 置 oom 标志。
//   · -fno-rtti：无任何 RTTI / 虚函数。
//   · 零全局可变状态：每个 Parse 调用拥有独立 Arena，释放即整体 free。
//
// 整棵 AST 一次性分配进同一个 Arena，解析结束（或出错）后整体释放。
// Arena 本身不抛异常（用 malloc，失败返回 nullptr）。

#pragma once

#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <cstring>

namespace hm {

class Arena {
 public:
  // 构造一个容量为 cap 字节的池。malloc 失败则 oom() 为 true，alloc 永远返回 nullptr。
  explicit Arena(size_t cap) : base_(nullptr), cap_(0), off_(0), oom_(false) {
    if (cap == 0) cap = 1;
    base_ = static_cast<uint8_t*>(malloc(cap));
    if (base_ == nullptr) {
      oom_ = true;
    } else {
      cap_ = cap;
    }
  }

  ~Arena() { free(base_); }

  // 禁止拷贝（池不可共享）。
  Arena(const Arena&) = delete;
  Arena& operator=(const Arena&) = delete;

  // 分配 n 字节，16 字节对齐。失败（含池满）返回 nullptr 并置 oom_。
  void* alloc(size_t n) {
    if (oom_) return nullptr;
    const size_t align = 16;
    size_t padded = (n + align - 1) & ~(align - 1);
    // 防溢出 + 防越界。
    if (padded < n) { oom_ = true; return nullptr; }
    if (off_ + padded < off_ || off_ + padded > cap_) {
      oom_ = true;
      return nullptr;
    }
    void* p = base_ + off_;
    off_ += padded;
    return p;
  }

  // 把 [src, src+len) 拷进池并补 '\0'，返回稳定指针（源失效也安全）。
  // 失败返回 nullptr。
  const char* intern(const char* src, size_t len) {
    char* p = static_cast<char*>(alloc(len + 1));
    if (p == nullptr) return nullptr;
    if (len) memcpy(p, src, len);
    p[len] = '\0';
    return p;
  }

  bool oom() const { return oom_; }
  size_t used() const { return off_; }
  size_t capacity() const { return cap_; }

 private:
  uint8_t* base_;
  size_t cap_;
  size_t off_;
  bool oom_;
};

// 在池里构造一个 T（placement new，构造函数不得分配/抛异常）。
template <class T, class... Args>
T* make_node(Arena& a, Args&&... args) {
  void* p = a.alloc(sizeof(T));
  if (p == nullptr) return nullptr;
  return new (p) T(static_cast<Args&&>(args)...);
}

}  // namespace hm
