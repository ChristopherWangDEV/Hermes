/*
 * cycle_x86_64.S —— x86-64 计数器读取（clang 集成汇编器）。
 *
 * rdtscp 给出 64 位不变 TSC（EDX:EAX），并且是**带序列化**的：
 * 它等前面的指令全部执行完再读。后面补 lfence 保证读之后的指令不被提到
 * 读之前，从而让连续两次读单调不减。
 *
 * 为什么不用 rdtsc + cpuid：cpuid 会破坏 eax/ebx/ecx/edx 且代价高两个数量级；
 * rdtscp 是这条路径的标准做法。也不需要用 RDTSCP 的 IA32_TSC_AUX。
 *
 * 同一份文件在 arm64 宿主机上由 build.sh 用 -arch x86_64 交叉汇编，
 * 保证 R-NF-03 的"两分支都被编译到"。
 */

#if defined(__APPLE__)
#define HM_SYM(x) _##x
#else
#define HM_SYM(x) x
#endif

	.text

	.globl	HM_SYM(hermes_hw_counter)
	.p2align 4
HM_SYM(hermes_hw_counter):
	rdtscp
	shlq	$32, %rdx
	orq	%rdx, %rax
	lfence
	ret

/* Linux 上缺这一段会让链接器把栈标成**可执行**（并给一条 warning）。
 * 我们没有任何地方需要在栈上执行代码，所以显式声明"不可执行"。
 * Mach-O 不认识这个段名，按平台守卫。 */
#if !defined(__APPLE__)
	.section .note.GNU-stack,"",@progbits
#endif
