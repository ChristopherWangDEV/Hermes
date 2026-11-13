// Package backend 是 Hermes 的**代码生成后端**接口（docs/NATIVE-BACKEND.md S0）。
//
// 设计要点（红线相关）：
//   - 后端**只吃 IR**（*ir.Doc），不碰语法。前端是语法唯一权威。
//   - 第一个实现是 C 后端：发 C 文本，驱用户机器上的 clang。clang 是 BSD/Apache
//     许可，不含 GPL 义务；Hermes **不内置**编译器（D-025 driver 模型）。
//   - 生成的 C **不含 IR JSON、不链 Go 运行时**（零解释器）：值、运算、控制流
//     都直接降为 C 代码，运行期只剩 libc。
//
// 为什么先 C 而不是 libLLVM：`clang -O2` 本身就是 LLVM 后端；直接链 libLLVM
// 只买到"少一次进程启动"，代价是 1.5 GB 工具链与版本钉死。验收标准与后端无关：
// 同一份 IR，产物行为必须与 `hermes run`（解释器）逐字一致。
package backend

import (
	"fmt"
	"sort"

	"hermes/internal/ir"
)

// Backend 把一个装配好的 IR unit 生成为某目标语言的**自包含源码**。
type Backend interface {
	// Name 是 `--backend=<name>` 里的名字。
	Name() string
	// Generate 返回可直接交给目标编译器（clang）的源码。
	Generate(doc *ir.Doc) ([]byte, error)
}

// registry 是已实现的后端表。新增后端只需在这里登记。
var registry = map[string]Backend{}

func register(b Backend) { registry[b.Name()] = b }

// For 按名字取后端；第二个返回值表示是否存在。
func For(name string) (Backend, bool) {
	b, ok := registry[name]
	return b, ok
}

// Names 返回已登记的后端名（排序，用于报错与帮助文本）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UsesNet 判断这个 unit 是否需要 glibhermesnet（g* 命名空间，含 Go runtime；
// 有 call_tool / call_model）。裸机目标由 internal/toolchain 直接拒绝该依赖。
// 只有需要的产物才链那个 14 MB 的 Go 归档；纯计算产物维持只链 libc。
func UsesNet(doc *ir.Doc) bool {
	for wi := range doc.Workflows {
		for i := range doc.Workflows[wi].Nodes {
			switch doc.Workflows[wi].Nodes[i].K {
			case "call_tool", "call_model":
				return true
			}
		}
	}
	return false
}

func init() {
	register(cBackend{})
}

// pickEntryWorkflow 选出生成本次可执行的 workflow。
//
// 当前 AOT 产物是"单入口可执行文件"，所以必须有且仅有一个可执行入口：
// 优先 `entry`，否则若只有一个 workflow 就用它，否则报错（不猜）。
func pickEntryWorkflow(doc *ir.Doc) (*ir.Workflow, error) {
	if len(doc.Workflows) == 0 {
		return nil, fmt.Errorf("backend: unit has no workflow to compile")
	}
	var entries []*ir.Workflow
	for i := range doc.Workflows {
		if doc.Workflows[i].IsEntry {
			entries = append(entries, &doc.Workflows[i])
		}
	}
	if len(entries) == 1 {
		return entries[0], nil
	}
	if len(entries) == 0 && len(doc.Workflows) == 1 {
		return &doc.Workflows[0], nil
	}
	if len(entries) > 1 {
		return nil, fmt.Errorf("backend: %d entry workflows; a single executable needs exactly one", len(entries))
	}
	return nil, fmt.Errorf("backend: no entry workflow in a multi-workflow unit; mark one `entry`")
}
