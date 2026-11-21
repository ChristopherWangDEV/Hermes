package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLoadIRHasNoGoFallback is the structural guard for the load-time
// validation cutover. All validation must live in the C++ validator; native
// LoadIR may only orchestrate C++ validation, diagnostic rendering and Doc
// binding. If someone reintroduces ir.Validate(doc) in that function, the
// build would still pass while a C++/Go serialization drift was silently
// hidden, so this guard parses the function body directly.
//
// parser.ParseFile is called without parser.ParseComments: comments do not
// enter the AST, so text in a comment cannot satisfy (or break) the guard.
func TestLoadIRHasNoGoFallback(t *testing.T) {
	// Resolve from this source file, not from the test process cwd: other tests
	// in this package chdir, so a cwd-relative path is not dependable.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the current test source file")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "internal", "native", "ir_load.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if ok && d.Name.Name == "LoadIR" {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatalf("%s: LoadIR not found", path)
	}
	if resultCount(fn) != 3 {
		t.Fatalf("LoadIR must return (*ir.Doc, []diag.Diag, error), got %d results", resultCount(fn))
	}
	third := fn.Type.Results.List[2].Type
	ident, ok := third.(*ast.Ident)
	if !ok || ident.Name != "error" {
		t.Fatalf("LoadIR third result must be error, got %T", third)
	}
	if fn.Body == nil {
		t.Fatal("LoadIR has no body")
	}

	// The body must not contain any ir.Validate(...) call.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Validate" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "ir" {
			t.Errorf("LoadIR reintroduced the Go fallback ir.Validate(...) at %s", fset.Position(call.Pos()))
		}
		return true
	})

	// The body must actually call the C++ bridge.
	hasIRValidate := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "IRValidate" {
			hasIRValidate = true
		}
		return true
	})
	if !hasIRValidate {
		t.Error("LoadIR no longer calls IRValidate: the C++ validator is not wired in")
	}

	// Both infrastructure failure anchors must remain explicit string literals
	// in the body, so bridge and decode failures cannot be silently swallowed.
	anchors := []string{
		"ir(native): C++ validator bridge: %w",
		"ir(native): cannot parse C++ validator diagnostics: %s",
	}
	seen := make(map[string]bool, len(anchors))
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, anchor := range anchors {
			if strings.Contains(lit.Value, anchor) {
				seen[anchor] = true
			}
		}
		return true
	})
	for _, anchor := range anchors {
		if !seen[anchor] {
			t.Errorf("LoadIR lost explicit error anchor %q", anchor)
		}
	}
}

func resultCount(fn *ast.FuncDecl) int {
	if fn.Type.Results == nil {
		return 0
	}
	return len(fn.Type.Results.List)
}
