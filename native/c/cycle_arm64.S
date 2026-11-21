/*
 * cycle_arm64.S —— arm64 计数器读取（clang 集成汇编器，不使用 nasm/gas）。
 *
 * 只做两件事，且必须只有两件事：
 *   1. 读 cntvct_el0  —— 频率不变的虚拟计数器，AArch64 上 24 MHz 典型
 *   2. 读 cntfrq_el0  —— 上面那个计数器的频率
 *
 * 前面加 isb：保证读到的计数器值在指令流上是有序的，否则乱序执行会让
 * "先读后读"出现倒退，R-NA-02 的单调性断言会随机红。
 *
 * 符号名刻意**不带** hm_ 前缀：它们是内部原语，不是 ABI。
 * 带 hm_ 前缀会污染 scripts/expected_symbols.txt 的双向守卫
 * （"Rust 只做 4 件事"的机器检查会因此失效）。
 */

#if defined(__APPLE__)
#define HM_SYM(x) _##x
#else
#define HM_SYM(x) x
#endif

	.text

	.globl	HM_SYM(hermes_hw_counter)
	.p2align 2
HM_SYM(hermes_hw_counter):
	isb
	mrs	x0, cntvct_el0
	ret

	.globl	HM_SYM(hermes_hw_counter_freq_asm)
	.p2align 2
HM_SYM(hermes_hw_counter_freq_asm):
	mrs	x0, cntfrq_el0
	ret

#if defined(__APPLE__)
	.subsections_via_symbols
#endif

/* Linux 上缺这一段会让链接器把栈标成**可执行**（并给一条 warning）。
 * 我们没有任何地方需要在栈上执行代码，所以显式声明"不可执行"。
 * Mach-O 不认识这个段名，按平台守卫。 */
#if !defined(__APPLE__)
	.section .note.GNU-stack,"",@progbits
#endif
