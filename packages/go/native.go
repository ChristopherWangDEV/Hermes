package hermes

// 进程内绑定（像 PyQt 封装 Qt 那样）：用 dlopen 加载 libhermesrt，
// 直接调 hermes_rt_run_file。不经过网络、不需要起服务。
//
// 为什么用 dlopen 而不是链接期 -lhermesrt：链接期要把库路径写死进二进制，
// 而这个 SDK 要能跟着"用户把 Hermes 装在哪"走。dlopen 在运行期按
// HERMES_HOME / 仓库布局查找，和 Python 的 ctypes 是同一个思路。

/*
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

// Go 调用 C 的函数指针需要一个薄包装：cgo 不能直接调用运行期取到的符号。
static int hermes_call_run_file(void* fn, const char* unit, const char* input,
                                size_t inputLen, char** out, char** err) {
  typedef int (*run_file_fn)(const char*, const char*, size_t, char**, char**);
  return ((run_file_fn)fn)(unit, input, inputLen, out, err);
}

static void hermes_call_free(void* fn, char* p) {
  typedef void (*free_fn)(char*);
  ((free_fn)fn)(p);
}
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

// NativeError 是原生层返回的错误。Code：2 = 输入/文件问题，3 = 执行失败。
type NativeError struct {
	Message string
	Code    int
}

func (e *NativeError) Error() string {
	return fmt.Sprintf("hermes: %s (code=%d)", e.Message, e.Code)
}

// NativeRuntime 是一次加载、多次执行的进程内运行时。
type NativeRuntime struct {
	unit string
	lib  unsafe.Pointer
	run  unsafe.Pointer
	free unsafe.Pointer
	once sync.Once
}

// OpenRuntime 加载运行时动态库。
//
// library 为空时按 HERMES_HOME → 仓库 build/ → 包旁 lib/ 的顺序查找。
func OpenRuntime(unit, library string) (*NativeRuntime, error) {
	path := library
	if path == "" {
		var err error
		path, err = FindLibrary("")
		if err != nil {
			return nil, err
		}
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	lib := C.dlopen(cpath, C.RTLD_NOW|C.RTLD_LOCAL)
	if lib == nil {
		return nil, &NativeError{Message: "加载 " + path + " 失败: " + C.GoString(C.dlerror())}
	}
	run := C.dlsym(lib, C.CString("hermes_rt_run_file"))
	fre := C.dlsym(lib, C.CString("hermes_rt_free"))
	if run == nil || fre == nil {
		C.dlclose(lib)
		return nil, &NativeError{Message: path + " 里缺 hermes_rt_run_file / hermes_rt_free"}
	}
	return &NativeRuntime{unit: unit, lib: lib, run: run, free: fre}, nil
}

// Close 卸载动态库。
func (r *NativeRuntime) Close() {
	r.once.Do(func() {
		if r.lib != nil {
			C.dlclose(r.lib)
			r.lib = nil
		}
	})
}

// Run 执行这个 unit。inputJSON 为空表示不需要输入。
func (r *NativeRuntime) Run(inputJSON string) (string, error) {
	return r.RunOn(r.unit, inputJSON)
}

// RunOn 执行指定的 unit。
func (r *NativeRuntime) RunOn(unit, inputJSON string) (string, error) {
	cunit := C.CString(unit)
	defer C.free(unsafe.Pointer(cunit))
	var cinput *C.char
	if inputJSON != "" {
		cinput = C.CString(inputJSON)
		defer C.free(unsafe.Pointer(cinput))
	}
	var out, errPtr *C.char
	code := C.hermes_call_run_file(r.run, cunit, cinput, C.size_t(len(inputJSON)), &out, &errPtr)
	defer func() {
		// 两块内存都由原生层分配，必须用它的 free 归还。
		if out != nil {
			C.hermes_call_free(r.free, out)
		}
		if errPtr != nil {
			C.hermes_call_free(r.free, errPtr)
		}
	}()
	if code != 0 {
		msg := "执行失败"
		if errPtr != nil {
			msg = C.GoString(errPtr)
		}
		return "", &NativeError{Message: msg, Code: int(code)}
	}
	if out == nil {
		return "", nil
	}
	return C.GoString(out), nil
}

// FindLibrary 按固定顺序找运行时动态库，找不到时列出试过的路径。
func FindLibrary(explicit string) (string, error) {
	name := "libhermesrt.so"
	if runtime.GOOS == "darwin" {
		name = "libhermesrt.dylib"
	} else if runtime.GOOS == "windows" {
		name = "hermesrt.dll"
	}
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", &NativeError{Message: "指定的库不存在: " + explicit}
	}
	tried := []string{}
	if home := os.Getenv("HERMES_HOME"); home != "" {
		tried = append(tried, filepath.Join(home, "lib", name))
	}
	tried = append(tried,
		filepath.Join("build", name),
		filepath.Join("..", "build", name),
		filepath.Join("..", "..", "build", name),
		filepath.Join("..", "..", "lib", name),
	)
	for _, p := range tried {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", &NativeError{Message: "找不到 " + name + "。已尝试：" +
		joinStrings(tried, ", ") + "。发布包应自带 lib/；开发时先跑 scripts/build.sh，或用 HERMES_HOME 指向安装根。"}
}

func joinStrings(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}
