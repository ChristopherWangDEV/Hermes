package engine

import "testing"

// TestRunErrorValueAlwaysCarriesStatus 守 `RunError.value()` 的**无条件** status。
//
// 为什么这条值得单独一个测试：静态契约说 `HttpError.status: int`（§7.1.1），
// 而 `field()` 对缺键是硬错 —— 不是 *RunError，`catch` 也接不住。于是"有条件地
// 写 status"会造成一个只在**状态码恰好是 0** 时才出现的运行期崩溃：
//
//	HttpError{Kind:"HttpError"}          // Status 缺省 = 0（body 序列化失败等路径）
//	catch (HttpError e) { e.status }     // 类型检查说 int，运行期说"没有这个字段"
//
// 这类缺陷端到端测不出来：真实 HTTP 失败几乎总是带非零状态码，`e.status == 404`
// 在两种实现下都正确。所以只能直接钉住 value() 的形状。
//
// 0 不是编造的值：§7.1.1 明写 `ModelError.status`（上游无状态码时为 0），
// 也就是说 0 就是规范给的"没有状态码"哨兵。
func TestRunErrorValueAlwaysCarriesStatus(t *testing.T) {
	cases := []struct {
		name   string
		re     RunError
		status int64
	}{
		{"HttpError 带 404", RunError{Kind: "HttpError", Msg: "x", Status: 404}, 404},
		{"HttpError 没有状态码", RunError{Kind: "HttpError", Msg: "x"}, 0},
		{"ModelError 没有状态码", RunError{Kind: "ModelError", Msg: "x"}, 0},
		{"ContractError", RunError{Kind: "ContractError", Msg: "x"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := c.re.value()
			got, ok := m["status"]
			if !ok {
				t.Fatalf("value() 必须始终带 status 键 —— 缺了它，`e.status` 会在运行期炸成"+
					"普通 error，而静态检查已经放行了这次访问：%#v", m)
			}
			n, isInt := got.(int64)
			if !isInt {
				t.Fatalf("status 必须是 int64（与 §7.1.1 的 int 对齐），实际 %T", got)
			}
			if n != c.status {
				t.Fatalf("status 应为 %d，实际 %d", c.status, n)
			}
		})
	}
}
