package diag_test

import (
	"fmt"

	"hermes/internal/diag"
)

// 这个 Example 是**报错格式的活文档**：`go test` 会真的渲染一遍并逐字比对输出。
// 所以它不是"注释里贴了一段样例"，而是"样例变了测试就红"。
//
// 对比一下同一个问题在 Python 里长什么样：
//
//	KeyError: 99
//
// 它没有说 99 是什么、在哪里、该是什么范围、怎么改。下面这条说了全部五件事。
func ExampleDiag_Render() {
	loc := diag.Loc{
		Module: "support.hbc",
		Path:   "workflows[0].nodes[3].kids[1]",
		Line:   7,
		Col:    9,
	}
	d := diag.New(
		diag.CodeDanglingRef,
		diag.SeverityError,
		loc,
		diag.LangEN,
		map[string]string{
			"what":  "workflows[0].nodes[3].kids[1]",
			"id":    "99",
			"table": "nodes",
			"range": "5 entries (0..4)",
			"path":  "workflows[0].nodes[3].kids[1]",
		},
	)
	d = d.WithNote(
		diag.Loc{Module: "support.hermes", Path: "workflows[0].nodes", Line: 12},
		"the workflow declares 5 nodes, ids 0 through 4",
	)
	fmt.Print(d.Render())
	// Output:
	// error[E_IR_DANGLING_REF]: workflows[0].nodes[3].kids[1] points at 99, but nodes has 5 entries (0..4)
	//   --> support.hbc
	//       at workflows[0].nodes[3].kids[1]
	//       line 7, col 9
	//    why: Every id in the IR's reference positions must index into the table it names. A dangling id means the IR was truncated, hand-edited, or produced by a frontend that is one version out of step.
	//   help: remove the reference at workflows[0].nodes[3].kids[1]
	//   help: or add the missing entry — the table must stay consistent with its references
	//   note: the workflow declares 5 nodes, ids 0 through 4 (support.hermes workflows[0].nodes line 12)
	//
}
