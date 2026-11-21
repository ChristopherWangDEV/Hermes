// Package linkpolicy owns the single, closed-set licence judgement for the
// libraries that must never enter a Hermes link. Both `hermes cc` (product-side
// dynamic-dependency check) and `hermes compile --link-*` (input-side gate plus
// product-side check) consult this package, so the red line has exactly one
// definition instead of two drifting copies.
//
// This is deliberately a **closed set**, not a heuristic: we never scan an
// arbitrary string for "looks like GPL" substrings. Every entry is an explicit
// library name with its licence, the reason it is forbidden, and an
// authoritative source. Adding a library is a reviewed, table-driven change.
package linkpolicy

import (
	"path/filepath"
	"strings"
)

// Rule is one entry of the closed set. Each field is required: a rule without
// a licence/reason/source is not auditable and would let the red line rot into
// an unexplainable string list.
type Rule struct {
	// Name is the lower-cased substring matched against filepath.Base(lib).
	Name string
	// Licence is the SPDX-style identifier (or the precise licence family).
	Licence string
	// Reason states why this library must not enter the link.
	Reason string
	// Source cites where the licence claim is documented.
	Source string
}

// ForbiddenRuntimes is the closed set of copyleft runtime libraries that must
// not enter a Hermes link. The red line is "which runtime entered the link",
// not "what the symbols look like" -- name mangling is both a false positive
// (a legal C++ TU using <string> produces mangled symbols) and a false
// negative (a static GPL archive's symbols are not mangled at all).
//
// Keep this list in lockstep with tests/guard/aot_link_test.go's
// forbiddenLibs and with the licence notes in cmd/hermes/ccUsage.
var ForbiddenRuntimes = []Rule{
	{
		Name:    "libstdc++",
		Licence: "GPL-3.0-or-later WITH GCC-Runtime-Library-Exception-3.1",
		Reason:  "the GNU C++ standard library runtime; the GPL C++ runtime must not enter the link (use libc++ instead)",
		Source:  "https://gcc.gnu.org/onlinedocs/libstdc++/faq.html#faq.license",
	},
	{
		Name:    "libsupc++",
		Licence: "GPL-3.0-or-later WITH GCC-Runtime-Library-Exception-3.1",
		Reason:  "the low-level GNU C++ runtime support library, shipped with libstdc++; same red line as libstdc++",
		Source:  "https://gcc.gnu.org/onlinedocs/libstdc++/faq.html#faq.license",
	},
	{
		Name:    "libgcc_s",
		Licence: "GPL-3.0-or-later WITH GCC-Runtime-Library-Exception-3.1",
		Reason:  "the GCC shared low-level runtime; linking it into a distributed artifact carries the GCC runtime exception obligations",
		Source:  "https://www.gnu.org/licenses/gcc-exception-3.1.en.html",
	},
	{
		Name:    "libgomp",
		Licence: "GPL-3.0-or-later WITH GCC-Runtime-Library-Exception-3.1",
		Reason:  "the GNU OpenMP runtime; part of the GCC runtime family and copyleft under the same exception",
		Source:  "https://gcc.gnu.org/onlinedocs/libgomp/",
	},
	{
		Name:    "libquadmath",
		Licence: "LGPL-2.1-or-later (GCC libquadmath)",
		Reason:  "the GCC quad-precision math runtime; copyleft, and static/dynamic linkage imposes obligations on the artifact",
		Source:  "https://gcc.gnu.org/onlinedocs/libquadmath/",
	},
}

// Match reports whether a single library name/path matches a rule of the
// closed set. Matching lowers the case and uses filepath.Base + Contains; it
// never inspects anything beyond the explicit entries above. The returned
// Rule is a copy from ForbiddenRuntimes (do not mutate it).
func Match(lib string) (Rule, bool) {
	base := strings.ToLower(filepath.Base(lib))
	for _, r := range ForbiddenRuntimes {
		if strings.Contains(base, r.Name) {
			return r, true
		}
	}
	return Rule{}, false
}

// FirstForbidden returns the first library in libs that matches a rule, its
// original spelling (for diagnostics), and the matched rule. An empty string
// means the link surface is clean. It is the shared entry point used by both
// `hermes cc` and `hermes compile`.
func FirstForbidden(libs []string) (string, Rule, bool) {
	for _, l := range libs {
		if r, ok := Match(l); ok {
			return l, r, true
		}
	}
	return "", Rule{}, false
}
