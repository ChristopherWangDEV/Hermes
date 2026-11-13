package toolchain

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"hermes/internal/linkpolicy"
)

// Refusal codes. One code per *cause*: the project's hard-won rule is that "one
// error code, two causes" is the same as having no cause at all, because a
// green test cannot tell which path it exercised. Every test asserts CodeOf.
const (
	CodeGoRuntimeBareMetal = "E3101" // g* (or legacy Go runtime) on a bare-metal target
	CodeDynamicBareMetal   = "E3102" // --link-dynamic on a bare-metal target
	CodeGoRuntimeName      = "E3103" // Go runtime archive not in the g* namespace
	CodeCrossNetArchive    = "E3104" // host-built Go archive into a cross-compiled artifact
	CodeCppABIBridge       = "E3105" // direct C++ ABI runtime link
	CodeBareMetalLinkShape = "E3106" // bare-metal --link-static that is not a .a
	CodeNetLibMissing      = "E3107" // needNet but no archive resolved
	CodeForbiddenRuntime   = "E3108" // licence closed set hit on the plan surface
)

// GoRuntimeBareMetalMessage is the exact, byte-for-byte compile-time error the
// user specified. It is printed verbatim (with a trailing newline) with no
// "hermes compile:" prefix, so nothing may be inserted in front of it.
const GoRuntimeBareMetalMessage = "[ERROR] Go runtime libraries (g*) cannot be linked for bare-metal targets. Please use pure C/Rust static libraries."

// Refusal is a named link refusal: Code is machine-assertable, Message is the
// user-facing text. Keeping them together is what stops a future refactor from
// collapsing two causes into one indistinguishable error string.
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// CodeOf extracts a Refusal code, or "" for an unrelated error.
func CodeOf(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func refusal(code, format string, a ...any) error {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, a...)}
}

// cppABIRuntimes is the CLOSED set of C++ ABI runtime library names. Linking
// one directly is forbidden: name mangling and STL memory layout are not
// portable across compilers. The supported route is a user-supplied shim
// compiled with `extern "C"`, which is itself a plain C ABI .a.
//
// NOTE: libc++ is licence-legal (Apache-2.0 WITH LLVM-exception) but still an
// ABI boundary, so it belongs here and NOT in linkpolicy.ForbiddenRuntimes.
// That separation is deliberate: licence reason and ABI reason must not share
// one error code.
var cppABIRuntimes = map[string]bool{
	"libstdc++": true,
	"libsupc++": true,
	"libc++":    true,
	"libc++abi": true,
	"libcxxrt":  true,
	"libcxx":    true,
}

// legacyGoRuntimeNames is a fail-closed safety net for the bare-metal ban.
// The g* prefix is the *declared* namespace, but a library that was never
// renamed must not be able to walk a Go runtime into firmware. Removing an
// entry turns TestLegacyGoRuntimeNameMutation red.
var legacyGoRuntimeNames = map[string]bool{
	"libhermesnet": true,
	"hermesnet":    true,
	"libhermesrt":  true,
	"hermesrt":     true,
}

// libStem returns the lower-cased library name with extension and soname
// version stripped, keeping the conventional "lib" prefix:
//
//	libstdc++.so.6  -> libstdc++
//	libc++.1.dylib  -> libc++
//	glibhermesnet.a -> glibhermesnet
//	libm.so.1       -> libm
func libStem(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if i := strings.Index(base, ".so"); i >= 0 {
		return base[:i]
	}
	for _, ext := range []string{".dylib", ".dll", ".a", ".lib"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}
	for {
		i := strings.LastIndex(base, ".")
		if i < 0 || i == len(base)-1 {
			break
		}
		digits := true
		for _, c := range base[i+1:] {
			if c < '0' || c > '9' {
				digits = false
				break
			}
		}
		if !digits {
			break
		}
		base = base[:i]
	}
	return base
}

// HasGoRuntimePrefix is the namespace contract: a library that contains a Go
// runtime MUST be named with a leading `g` (glibhermesnet.a, gnet.a, ...).
// This is a declaration, not a heuristic scan of file contents. Pure C/Rust
// libraries must not occupy this prefix; the compiler is entitled to treat
// every g* library as Go-runtime-bearing.
func HasGoRuntimePrefix(lib string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(lib)), "g")
}

// IsGoRuntimeLib is the fail-closed detector used by the bare-metal ban: the
// g* namespace OR a known legacy Go-runtime name. Both detectors are needed:
// the prefix is the contract, the legacy set stops an un-renamed archive from
// slipping through.
func IsGoRuntimeLib(lib string) bool {
	if HasGoRuntimePrefix(lib) {
		return true
	}
	return legacyGoRuntimeNames[libStem(lib)]
}

// CheckPureCABI refuses a direct C++ ABI runtime link and tells the user the
// supported alternative. The rest of the pure-C-ABI guarantee is structural:
// the driver compiles main.c with clang -std=c99 (never clang++ / -x c++),
// every emitted extern prototype therefore has unmangled C linkage, and no
// C++ runtime flag is ever added to the plan. A mangled-only archive simply
// fails to resolve those unmangled symbols at link time.
func CheckPureCABI(flag, lib string) error {
	stem := libStem(lib)
	if cppABIRuntimes[stem] {
		return refusal(CodeCppABIBridge,
			"hermes compile: %s %q is a C++ ABI runtime (%s). "+
				"Hermes links pure C ABI only: name mangling and STL layout are not portable across compilers. "+
				"Provide a C bridge compiled with extern \"C\" and link that instead.", flag, lib, stem)
	}
	return nil
}

// Input is everything BuildLinkPlan needs; it contains no filesystem state of
// its own, so the plan builder stays a pure function.
type Input struct {
	Target      Target
	NeedNet     bool     // backend.UsesNet(doc): generated C calls hermes_net_*
	NetLib      string   // resolved g* archive path (may be empty)
	StaticLibs  []string // --link-static
	DynamicLibs []string // --link-dynamic
}

// Plan is the authoritative link surface. Nothing outside it may be appended
// to the clang command line: productLinkGate checks its invariants afterwards.
type Plan struct {
	StaticLibs    []string // .a archives, in link order
	DynamicLibs   []string // .so/.dylib (host only)
	Frameworks    []string // darwin frameworks, already in flag/value pairs
	GoRuntimeLibs []string // every library known to carry a Go runtime
	UsedNet       bool     // the built-in g* network archive entered the artifact
}

// BuildLinkPlan is the ONE place that decides which libraries enter an AOT
// artifact. Every target-dependent rule lives here; the caller must not add
// libraries of its own afterwards.
//
//   - Pure C ABI is checked first, for every input, on every target.
//   - Bare-metal: no Go runtime, no built-in network archive, no shared
//     library, no framework, only pure C/Rust static .a. Violations of the
//     g* rule produce the exact GoRuntimeBareMetalMessage.
//   - Host: g* is allowed; the built-in network archive MUST carry the g*
//     prefix and MUST be host-native (a Go c-archive built for another
//     architecture cannot be linked into a cross-compiled artifact).
//   - Darwin: any Go-runtime-bearing archive — the built-in network archive
//     OR a user-supplied g* c-archive passed via --link-static/--link-dynamic —
//     adds the CoreFoundation/Security frameworks the Go runtime needs.
//     A non-darwin host target and every bare-metal target carry no frameworks.
func BuildLinkPlan(in Input) (Plan, error) {
	var p Plan

	// 1. Pure C ABI — target-independent, before any classification.
	for _, libs := range []struct {
		flag string
		set  []string
	}{
		{"--link-static", in.StaticLibs},
		{"--link-dynamic", in.DynamicLibs},
	} {
		for _, lib := range libs.set {
			if err := CheckPureCABI(libs.flag, lib); err != nil {
				return Plan{}, err
			}
		}
	}

	// 2. Licence closed set again, on the *plan* surface (defence in depth:
	//    linkInputGate already ran on --link-*, but the built-in net archive is
	//    a separate input path). Same shared definition, no second list.
	allInputs := append(append([]string{}, in.StaticLibs...), in.DynamicLibs...)
	if in.NetLib != "" {
		allInputs = append(allInputs, in.NetLib)
	}
	if bad, rule, ok := linkpolicy.FirstForbidden(allInputs); ok {
		return Plan{}, refusal(CodeForbiddenRuntime,
			"hermes compile: %s is a %s runtime (%s): %s (source: %s)",
			bad, rule.Name, rule.Licence, rule.Reason, rule.Source)
	}

	// 3. Hardware layer: structurally sever every path to the Go runtime.
	if in.Target.IsBareMetal() {
		if in.NeedNet {
			return Plan{}, &Refusal{Code: CodeGoRuntimeBareMetal, Message: GoRuntimeBareMetalMessage}
		}
		// g* 检查覆盖 static 与 dynamic 两组：用户拿 --link-dynamic 塞一个
		// gnet.so 也必须先报用户指定的那条 [ERROR]，不能被后面泛化的
		// "no dynamic loader" 抢走原因（一个错误码两种原因等于没有原因）。
		for _, lib := range append(append([]string{}, in.StaticLibs...), in.DynamicLibs...) {
			if IsGoRuntimeLib(lib) {
				return Plan{}, &Refusal{Code: CodeGoRuntimeBareMetal, Message: GoRuntimeBareMetalMessage}
			}
		}
		for _, lib := range in.StaticLibs {
			if !strings.HasSuffix(strings.ToLower(lib), ".a") {
				return Plan{}, refusal(CodeBareMetalLinkShape,
					"hermes compile: bare-metal target %s may link static .a archives only; "+
						"--link-static %q is not a static archive. Rebuild it as a pure C/Rust .a.",
					in.Target.Raw, lib)
			}
		}
		if len(in.DynamicLibs) > 0 {
			return Plan{}, refusal(CodeDynamicBareMetal,
				"hermes compile: bare-metal target %s has no dynamic loader; "+
					"--link-dynamic is forbidden (got %q). Use --link-static with a pure C/Rust .a.",
				in.Target.Raw, in.DynamicLibs[0])
		}
		p.StaticLibs = append(p.StaticLibs, in.StaticLibs...)
		return p, nil // GoRuntimeLibs / DynamicLibs / Frameworks stay empty
	}

	// 4. Software layer. g* is permitted, but the namespace must be honoured.
	if in.NeedNet {
		if in.NetLib == "" {
			return Plan{}, refusal(CodeNetLibMissing,
				"hermes compile: this program calls tools/models and needs the Go network archive; "+
					"build it with: go build -buildmode=c-archive -o build/glibhermesnet.a ./cmd/hermesnet")
		}
		if !HasGoRuntimePrefix(in.NetLib) {
			return Plan{}, refusal(CodeGoRuntimeName,
				"hermes compile: %q carries a Go runtime but is not in the g* namespace. "+
					"Rename it to glibhermesnet.a (or gnet.a) so the target gate can see it; "+
					"all Go-runtime libraries must start with 'g'.", in.NetLib)
		}
		if !in.Target.Native {
			return Plan{}, refusal(CodeCrossNetArchive,
				"hermes compile: the built-in Go network archive is built for the host "+
					"and cannot be linked into an artifact cross-compiled to %s. "+
					"Build a g* c-archive for %s and pass it with --link-static, "+
					"or compile this unit natively.", in.Target.Raw, in.Target.Raw)
		}
		// The network archive goes first: static archives are resolved in
		// command-line order and it must be present when hermes_net_* is seen.
		p.StaticLibs = append(p.StaticLibs, in.NetLib)
		p.GoRuntimeLibs = append(p.GoRuntimeLibs, in.NetLib)
		p.UsedNet = true
	}

	p.StaticLibs = append(p.StaticLibs, in.StaticLibs...)
	p.DynamicLibs = append(p.DynamicLibs, in.DynamicLibs...)
	seen := make(map[string]bool, len(p.GoRuntimeLibs))
	for _, lib := range p.GoRuntimeLibs {
		seen[lib] = true
	}
	for _, lib := range append(append([]string{}, p.StaticLibs...), p.DynamicLibs...) {
		if !seen[lib] && IsGoRuntimeLib(lib) {
			p.GoRuntimeLibs = append(p.GoRuntimeLibs, lib)
			seen[lib] = true
		}
	}
	// Frameworks are decided from the *reconciled* Go-runtime surface, not from
	// NeedNet alone: a user-supplied g* c-archive with NeedNet=false still pulls
	// crypto/x509's CoreFoundation/Security symbols into the clang link.
	if in.Target.GOOS == "darwin" && len(p.GoRuntimeLibs) > 0 {
		p.Frameworks = []string{"-framework", "CoreFoundation", "-framework", "Security"}
	}
	return p, nil
}
