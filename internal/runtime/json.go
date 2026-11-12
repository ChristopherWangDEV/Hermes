package runtime

import (
	"bytes"
	"encoding/json"
)

// jsonUnmarshalNumber 用 UseNumber 解码：整数保持整数，不被悄悄变成 float64
// —— IR 禁浮点，而运行期一旦把 1 变成 1.0，回写 JSON 时就会污染下游。
func jsonUnmarshalNumber(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}
