// Package toolchain is the single decision point for `hermes compile`'s
// target-aware link surface.
//
// Before this package existed, cmd_compile.go mixed three unrelated facts in
// one place: (a) what the target is, (b) which libraries may enter the link,
// and (c) whether a Go runtime is allowed in the artifact. That made the
// hardware/software isolation a comment instead of a machine-checked rule:
// --target was a bare GOOS string, cross-compilation was refused outright, and
// the Go network archive was appended by an unconditional `if needNet`.
//
// Now exactly one place classifies a target and exactly one place builds the
// link plan. cmd_compile.go only supplies inputs and consumes the plan. Adding
// a target class or a runtime namespace is a reviewed, table-driven change in
// this package, guarded by mutation tests.
package toolchain

import (
	"fmt"
	"sort"
	"strings"
)

// Class is the runtime-isolation class of a compile target. It is NOT a
// synonym for "which OS": a bare-metal target has no hosted runtime at all,
// so no library carrying a Go runtime may ever enter its link line.
type Class uint8

const (
	// ClassUnknown is never returned by ParseTarget; it exists so a zero-value
	// Target cannot silently behave like a hosted target.
	ClassUnknown Class = iota
	// ClassHost: software layer / hosted target. A g* Go-runtime archive may
	// be linked (cloud concurrency), and hosted libc/frameworks are available.
	ClassHost
	// ClassBareMetal: hardware layer / freestanding target. Absolutely no g*
	// library, no Go runtime, no dynamic library. Only pure C/Rust static .a.
	ClassBareMetal
)

func (c Class) String() string {
	switch c {
	case ClassHost:
		return "host"
	case ClassBareMetal:
		return "bare-metal"
	default:
		return "unknown"
	}
}

// Target is a parsed LLVM target triple plus the two derived facts the driver
// needs: its isolation Class and its normalised GOOS (for licence gates).
type Target struct {
	Raw      string // the user's original --target spelling
	Triple   string // canonical value passed to clang -target; "" means native
	Arch     string // triple arch field, e.g. "thumbv7em", "x86_64"
	Vendor   string // triple vendor field, e.g. "apple", "unknown"
	OS       string // triple OS field, e.g. "none", "linux", "darwin"
	Env      string // triple environment field, e.g. "eabi", "gnu"
	GOOS     string // normalised GOOS for staticLibcGate / framework selection
	Class    Class
	Native   bool // true: no -target needed, host-built Go archives are valid
	Explicit bool // true: the user passed --target
}

// IsBareMetal reports whether the hardware-layer isolation rules apply.
func (t Target) IsBareMetal() bool { return t.Class == ClassBareMetal }

// ClangTargetArgs returns the -target flags to inject into the clang command.
// A native build deliberately gets none: clang's own default triple is already
// correct and passing -target would change the driver's sysroot/linker choice
// on Apple platforms.
func (t Target) ClangTargetArgs() []string {
	if t.Native || t.Triple == "" {
		return nil
	}
	return []string{"-target", t.Triple}
}

// hostedOS is the CLOSED set of OS fields we know how to link a hosted artifact
// for, mapped to the normalised GOOS used by the licence gates. Anything not in
// this table is refused, never guessed: misclassifying a freestanding target as
// hosted is exactly how a Go runtime would leak into firmware. Adding an entry
// is a reviewed change (TestHostedOSClosedSetMutation keeps the table wired).
var hostedOS = map[string]string{
	"darwin":    "darwin",
	"macos":     "darwin",
	"ios":       "darwin",
	"linux":     "linux",
	"android":   "linux",
	"windows":   "windows",
	"win32":     "windows",
	"freebsd":   "freebsd",
	"netbsd":    "netbsd",
	"openbsd":   "openbsd",
	"dragonfly": "dragonfly",
}

// bareMetalOS is the CLOSED set of OS fields that mean "no hosted runtime".
// `none` covers the 4-field form riscv32imc-unknown-none-elf.
var bareMetalOS = map[string]bool{
	"none": true,
}

// bareMetalEnvMarkers names the 3-field ARM/GNU bare-metal shorthand
// `arch-none-<env>` (thumbv7em-none-eabi, arm-none-eabihf). In that spelling
// the OS field is OMITTED and implicitly "none"; the third field is the
// environment. Treating "eabi" as an OS would misclassify exactly the target
// the user named as the hardware-layer example.
var bareMetalEnvMarkers = map[string]bool{
	"eabi":   true,
	"eabihf": true,
	"elf":    true,
}

// hostedEnvMarkers names the `arch-<os>-<env>` shorthand for hosted ARM/linux
// triples (arm-linux-gnueabihf). Keeping it separate from bareMetalEnvMarkers
// is what stops a hosted Linux target from being called bare-metal.
var hostedEnvMarkers = map[string]bool{
	"gnu":        true,
	"gnueabi":    true,
	"gnueabihf":  true,
	"musl":       true,
	"musleabi":   true,
	"musleabihf": true,
	"msvc":       true,
	"android":    true,
}

// archGOARCH maps a triple arch field onto runtime.GOARCH so Native can be
// computed without guessing. Unknown arches simply make the target non-native;
// that is safe (it only disables host-built Go archives).
var archGOARCH = map[string]string{
	"x86_64":      "amd64",
	"amd64":       "amd64",
	"aarch64":     "arm64",
	"arm64":       "arm64",
	"i386":        "386",
	"i486":        "386",
	"i586":        "386",
	"i686":        "386",
	"x86":         "386",
	"riscv64":     "riscv64",
	"loongarch64": "loong64",
}

// ParseTarget turns a --target value into a Target. goos/goarch are passed in
// (rather than read from runtime inside) so the classifier is a pure function
// and can be tested against triples from other machines.
//
// Accepted forms:
//   - "" or the current GOOS: native hosted build, clang gets no -target.
//   - a bare GOOS other than the current one: REFUSED, with a message asking
//     for a full triple. A GOOS alone carries no arch/ABI information, and
//     pretending otherwise is how cross-compilation silently produced a
//     wrong-architecture artifact before this refactor.
//   - a full triple <arch>-<vendor>-<os>[-<env>]: classified by a closed set.
//     OS "none" is bare-metal; known hosted OS names are host; anything else is
//     a named fail-closed error.
func ParseTarget(raw, goos, goarch string) (Target, error) {
	if raw == "" {
		return Target{GOOS: goos, Arch: goarch, Class: ClassHost, Native: true}, nil
	}
	t := Target{Raw: raw, Explicit: true, Triple: raw}

	if !strings.Contains(raw, "-") {
		if _, known := hostedOS[strings.ToLower(raw)]; !known {
			return Target{}, fmt.Errorf(
				"hermes compile: --target %q is not a known GOOS and not a target triple; "+
					"use a full LLVM triple such as x86_64-unknown-linux-gnu, or one of the supported GOOS values",
				raw)
		}
		if strings.ToLower(raw) != goos {
			return Target{}, fmt.Errorf(
				"hermes compile: cross-compiling to GOOS %q needs a full LLVM target triple "+
					"(for example x86_64-unknown-linux-gnu or thumbv7em-none-eabi); "+
					"a bare GOOS carries no arch or ABI information", raw)
		}
		t.OS = strings.ToLower(raw)
		t.GOOS = goos
		t.Class = ClassHost
		t.Native = true
		t.Triple = "" // native: let clang use its own default triple
		return t, nil
	}

	parts := strings.Split(raw, "-")
	if len(parts) < 3 || len(parts) > 4 {
		return Target{}, fmt.Errorf(
			"hermes compile: --target %q is not an LLVM target triple; "+
				"expected <arch>-<vendor>-<os>[-<env>] (3 or 4 fields)", raw)
	}
	for _, p := range parts {
		if p == "" {
			return Target{}, fmt.Errorf("hermes compile: --target %q has an empty triple field", raw)
		}
	}
	t.Arch = strings.ToLower(parts[0])
	t.Vendor = strings.ToLower(parts[1])
	t.OS = strings.ToLower(parts[2])
	if len(parts) == 4 {
		t.Env = strings.ToLower(parts[3])
	} else if _, isHostedOS := hostedOS[t.OS]; !isHostedOS {
		// 3-field shorthand. Two real-world shapes exist and they mean opposite
		// things for the isolation class, so both are matched explicitly:
		//   arch-none-eabi     -> OS "none", env "eabi"   (bare metal)
		//   arch-linux-gnueabihf -> OS "linux", env ...   (hosted)
		switch {
		case t.Vendor == "none" && bareMetalEnvMarkers[t.OS]:
			t.Env = t.OS
			t.OS = "none"
		case hostedOS[t.Vendor] != "" && hostedEnvMarkers[t.OS]:
			t.Env = t.OS
			t.OS = t.Vendor
			t.Vendor = "unknown"
		}
	}

	norm, hosted := hostedOS[t.OS]
	switch {
	case bareMetalOS[t.OS]:
		t.Class = ClassBareMetal
		t.GOOS = "none"
		t.Native = false // a firmware target is never the host
	case hosted:
		t.Class = ClassHost
		t.GOOS = norm
		t.Native = norm == goos && archGOARCH[t.Arch] == goarch
	default:
		known := make([]string, 0, len(hostedOS)+1)
		for k := range hostedOS {
			known = append(known, k)
		}
		known = append(known, "none")
		sort.Strings(known)
		return Target{}, fmt.Errorf(
			"hermes compile: --target %q has OS field %q which is not in the closed set [%s]; "+
				"refusing to guess whether it is hosted or bare-metal, because that guess decides "+
				"whether a Go runtime may enter the artifact", raw, t.OS, strings.Join(known, ", "))
	}
	return t, nil
}
