package archive

import (
	"bytes"
	"encoding/json"
	"strconv"
)

type decoder struct{ d *json.Decoder }

func newDecoder(b []byte) *decoder {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return &decoder{d: d}
}

func (x *decoder) decode(v any) error { return x.d.Decode(v) }

// toInt 读索引里的整数。用 json.Number 解，避免 int → float64 的悄悄变形。
func toInt(v any) int {
	switch t := v.(type) {
	case json.Number:
		i, err := strconv.Atoi(t.String())
		if err != nil {
			return 0
		}
		return i
	case float64:
		return int(t)
	}
	return 0
}
