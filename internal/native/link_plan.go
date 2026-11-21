// link_plan.go —— 链接面决策的 cgo 绑定（C++ 实现）。
package native

/*
#include <stdlib.h>
int hm_target_parse(const char* raw, const char* goos, const char* goarch, char* out_json,
                    size_t cap, size_t* out_len, char* out_err, size_t err_cap, size_t* err_len);
int hm_link_plan(const char* target_json, int need_net, const char* net_lib, const char* static_csv,
                 const char* dynamic_csv, char* out_json, size_t cap, size_t* out_len);
int hm_static_libc_gate(const char* goos, int is_static, char* out, size_t cap, size_t* out_len);
int hm_link_input_gate(const char* flag, const char* lib, char* out, size_t cap, size_t* out_len);
int hm_product_link_gate(const char* out_path, int used_dynamic, const char* bare_metal_json,
                         const char* plan_json, char* out_msg, size_t cap, size_t* out_len);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"strings"
	"unsafe"
)

// LinkPlan 是 C++ 侧算出的链接面。
type LinkPlan struct {
	Static     []string `json:"-"`
	Dynamic    []string `json:"-"`
	Frameworks []string `json:"-"`
	GoRuntime  []string `json:"-"`
	UsedNet    bool     `json:"used_net"`
}

// LinkRefusal 是带码的拒绝（一个码一种原因）。
type LinkRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *LinkRefusal) Error() string { return r.Message }

// PlanLink 调 C++ 的 BuildLinkPlan 等价物。
// target 必须是 ParseTarget 的结果（本函数内部会重新序列化）。
func PlanLink(t TargetInfo, needNet bool, netLib string, statics, dynamics []string) (LinkPlan, error) {
	tj, err := json.Marshal(t)
	if err != nil {
		return LinkPlan{}, err
	}
	cTarget := C.CString(string(tj))
	cNet := C.CString(netLib)
	cStatic := C.CString(strings.Join(statics, ","))
	cDyn := C.CString(strings.Join(dynamics, ","))
	defer C.free(unsafe.Pointer(cTarget))
	defer C.free(unsafe.Pointer(cNet))
	defer C.free(unsafe.Pointer(cStatic))
	defer C.free(unsafe.Pointer(cDyn))

	buf := make([]byte, 8192)
	var n C.size_t
	nn := 0
	if needNet {
		nn = 1
	}
	rc := C.hm_link_plan(cTarget, C.int(nn), cNet, cStatic, cDyn,
		(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)
	switch int(rc) {
	case 0, 1:
		var raw struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			Static     string `json:"static"`
			Dynamic    string `json:"dynamic"`
			Frameworks string `json:"frameworks"`
			GoRuntime  string `json:"go_runtime"`
			UsedNet    bool   `json:"used_net"`
		}
		if err := json.Unmarshal(buf[:int(n)-1], &raw); err != nil {
			return LinkPlan{}, err
		}
		if int(rc) == 1 {
			return LinkPlan{}, &LinkRefusal{Code: raw.Code, Message: raw.Message}
		}
		return LinkPlan{
			Static:     nonEmpty(strings.Split(raw.Static, ",")),
			Dynamic:    nonEmpty(strings.Split(raw.Dynamic, ",")),
			Frameworks: nonEmpty(strings.Split(raw.Frameworks, " ")),
			GoRuntime:  nonEmpty(strings.Split(raw.GoRuntime, ",")),
			UsedNet:    raw.UsedNet,
		}, nil
	default:
		return LinkPlan{}, errors.New("linkplan(native): invalid argument")
	}
}

func nonEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// StaticLibcGate 是 Linux 静态链 glibc 的许可闸门（C++ 实现）。
func StaticLibcGate(goos string, isStatic bool) error {
	cGoos := C.CString(goos)
	defer C.free(unsafe.Pointer(cGoos))
	buf := make([]byte, 2048)
	var n C.size_t
	nn := 0
	if isStatic {
		nn = 1
	}
	if int(C.hm_static_libc_gate(cGoos, C.int(nn), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)) == 1 {
		return errors.New(string(buf[:int(n)-1]))
	}
	return nil
}

// LinkInputGate 校验一个 --link-* 路径（C++ 实现）。
// 拒绝时返回带码的 *LinkRefusal（一个码一种原因）。
func LinkInputGate(flag, lib string) error {
	cFlag := C.CString(flag)
	cLib := C.CString(lib)
	defer C.free(unsafe.Pointer(cFlag))
	defer C.free(unsafe.Pointer(cLib))
	buf := make([]byte, 2048)
	var n C.size_t
	if int(C.hm_link_input_gate(cFlag, cLib, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n)) != 1 {
		return nil
	}
	var raw LinkRefusal
	if err := json.Unmarshal(buf[:int(n)-1], &raw); err != nil {
		return errors.New(string(buf[:int(n)-1]))
	}
	return &raw
}

// ProductLinkGate 是产物侧兜底闸门（C++ 实现）。
func ProductLinkGate(outPath string, usedDynamic, bareMetal bool, planJSON string) error {
	cOut := C.CString(outPath)
	cPlan := C.CString(planJSON)
	cBare := C.CString("0")
	if bareMetal {
		cBare = C.CString("1")
	}
	defer C.free(unsafe.Pointer(cOut))
	defer C.free(unsafe.Pointer(cPlan))
	defer C.free(unsafe.Pointer(cBare))
	buf := make([]byte, 2048)
	var n C.size_t
	nn := 0
	if usedDynamic {
		nn = 1
	}
	rc := int(C.hm_product_link_gate(cOut, C.int(nn), cBare, cPlan,
		(*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n))
	switch rc {
	case 0:
		return nil
	case 1:
		return errors.New(string(buf[:int(n)-1]))
	default:
		return errors.New("productgate(native): invalid argument")
	}
}
