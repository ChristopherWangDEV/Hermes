// Package std —— 标准库实现变体的**唯一事实源与命名约定**。
//
// 产品定位（见 docs/STD-VARIANT.md）：Hermes 语言主体 = LLVM/Rust/C/C++；
// 标准库是 App 层可选件。当某成员的实现涉及 Go 特性且 Go 版更优时，App 层
// 可用其 **g*/G* 版**（Go 实现）；用户也可选**非 Go 版**（纯 C/Rust 主体实现）。
//
// 命名约定（g*/G*）：Go 版实现的名字一律带 g/G 前缀，与纯 C/Rust 静态库做
// 命名空间隔离（与 internal/toolchain 的 g* 策略一致）：
//
//	std/net     → Go 版 gnet      → 非 Go 版 net
//	std/model   → Go 版 gmodel    → 非 Go 版 model
//	std/runtime → Go 版 gruntime  → 非 Go 版 runtime
//	std/json    → Go 版 gjson     → 非 Go 版 json
//
// 默认变体 = go（即今天的行为）；选 c 才走非 Go 实现。这条默认值是向后兼容的
// 硬保证：不显式选择时，解析、哈希、运行期行为都与今天逐字节一致。
package std

import "os"

// Variant 是某个 std 成员的实现语言标签。
type Variant string

const (
	// VariantGo 是现有 Go 实现（cmd/hermesnet 经 cgo 暴露的 C ABI）。
	VariantGo Variant = "go"
	// VariantC 是纯 C 主体实现（不拖 Go runtime）。
	VariantC Variant = "c"
)

// DefaultVariant 是 App 层未显式选择时的变体，等于今天的行为，保证向后兼容。
const DefaultVariant = VariantGo

// Impl 描述一个 std 成员的某语言实现。
type Impl struct {
	Variant Variant
	Name    string // g*/G* 命名空间下的实现名（go 版）或成员本名（非 go 版）
	GoImpl  bool   // 是否 Go 实现（用于 g* 命名空间隔离判断）
}

// impls 是各 std 成员的实现清单。新增成员或新变体只改这里。
// 键是去前缀后的成员名（net/model/runtime/json）。
var impls = map[string]map[Variant]Impl{
	"net":     {VariantGo: {VariantGo, "gnet", true}, VariantC: {VariantC, "net", false}},
	"model":   {VariantGo: {VariantGo, "gmodel", true}, VariantC: {VariantC, "model", false}},
	"runtime": {VariantGo: {VariantGo, "gruntime", true}, VariantC: {VariantC, "runtime", false}},
	"json":    {VariantGo: {VariantGo, "gjson", true}, VariantC: {VariantC, "json", false}},
}

// ImplOf 返回某成员在指定变体下的实现描述；变体不存在返回 (Impl{}, false)。
//
// 这是"按选择取实现"的**唯一判定点**：调用方（internal/link、internal/include）
// 不得自行拼实现名，必须走这里。默认变体（go）返回与今天一致的结果。
func ImplOf(mod string, v Variant) (Impl, bool) {
	m, ok := impls[mod]
	if !ok {
		return Impl{}, false
	}
	im, ok := m[v]
	if !ok {
		return Impl{}, false
	}
	return im, true
}

// IsKnownMember 报告 name（可带 std/ 前缀）是否是内置 std 成员。
func IsKnownMember(name string) bool {
	name = trimStdPrefix(name)
	_, ok := impls[name]
	return ok
}

// Variants 返回某成员提供的所有变体（按固定顺序），用于文档与测试枚举。
func Variants(mod string) []Variant {
	m, ok := impls[mod]
	if !ok {
		return nil
	}
	out := make([]Variant, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	return out
}

// SelectedVariant 读取 App 层选择的标准库变体。
//
// 通过环境变量 HERMES_STD_VARIANT 选择；缺省或非法值一律回退到 DefaultVariant（go），
// 即今天的行为。运行期切换，不写进 IR、不改 requires 的哈希，因此不影响供应链校验。
func SelectedVariant() Variant {
	switch os.Getenv("HERMES_STD_VARIANT") {
	case "c", "C":
		return VariantC
	default:
		return DefaultVariant
	}
}

// BudgetProbe 是三维预算探针（镜像 internal/budget 的 Upper/Limits）。
type BudgetProbe struct {
	In, Out, MicroUSD int64
}

// exceedsGo 是 std/runtime 预算检查的纯 Go 实现，与 internal/budget.exceeds 逐字一致。
// <=0 的维度视为不限。
func exceedsGo(used, up, lim BudgetProbe) bool {
	if lim.In > 0 && used.In+up.In > lim.In {
		return true
	}
	if lim.Out > 0 && used.Out+up.Out > lim.Out {
		return true
	}
	if lim.MicroUSD > 0 && used.MicroUSD+up.MicroUSD > lim.MicroUSD {
		return true
	}
	return false
}

func trimStdPrefix(name string) string {
	const p = "std/"
	if len(name) >= len(p) && name[:len(p)] == p {
		return name[len(p):]
	}
	return name
}
