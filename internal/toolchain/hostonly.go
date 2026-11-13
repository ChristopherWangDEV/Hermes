// hostonly.go —— 宿主 OS 专属语义的编译期闸门。
//
// 为什么需要它：`sleep` / `until` / `emit ... to` 这三样都依赖**宿主 OS**：
// 前者要 nanosleep/Sleep，中者要条件变量与线程调度，后者要按上下文句柄做
// 间接调用（裸机上没有"上下文句柄"这种东西，槽表就是静态地址）。
// 裸机目标（如 thumbv7em-none-eabi）上它们没有诚实实现，只能在编译期拒绝。
//
// 判据放在**链接面**而不是 C++ 前端：前端不知道 target，它只产 IR；
// 目标分类是 toolchain 的事，两者职责不混。
package toolchain

// CodeHostOnlyNode 是"裸机上用了宿主 OS 专属语义"的错误码。
const CodeHostOnlyNode = "E3109"

// HostOnlyMessage 是定稿文案：用户原话，逐字不改。
const HostOnlyMessage = "[ERROR] Blocking synchronization or network routing requires a host OS."

// HostOnlyKind 描述一个"只有宿主 OS 才能实现"的 IR 节点。
// 不用 ir 包的类型是为了让 toolchain 不反向依赖 ir（二者职责相反）。
type HostOnlyKind struct {
	Kind   string // IR 节点 kind："sleep" / "until" / "emit"
	Route  int    // 仅 emit 有意义：2 = 定向路由（to <ctx>）
	Signal string // 仅作诊断里点名用
}

// CheckHostOnlyNodes 在裸机目标上拒绝宿主 OS 专属节点。
//
// 为什么不检查 route==1（频道路由）：频道是**编译期哈希 + 整数比较**，
// 没有任何串口/网络调用，裸机上照样能做。真正越界的是 route==2
// （定向路由需要一个活着的上下文句柄）与 sleep/until（需要调度器）。
func CheckHostOnlyNodes(bareMetal bool, nodes []HostOnlyKind) error {
	if !bareMetal {
		return nil
	}
	for _, n := range nodes {
		hostOnly := n.Kind == "sleep" || n.Kind == "until" ||
			(n.Kind == "emit" && n.Route == 2)
		if !hostOnly {
			continue
		}
		return &Refusal{Code: CodeHostOnlyNode, Message: HostOnlyMessage}
	}
	return nil
}
