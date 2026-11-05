// Package i18npack 从源码里提取 tr("en", "zh") 的键。
//
// 提取逻辑只写一份：CLI 的 `hermes i18n` 与开发工具 `tools/geni18n` 都调它。
// 两份实现迟早会分叉，而分叉后的表现是"工具生成的包和 CI 检查的包不一致"。
package i18npack

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Extract 返回 en → zh 的映射（只看 tr 调用点的两个字面量参数）。
func Extract(dir string) (map[string]string, error) {
	out := map[string]string{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(af, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			// 单参数（新写法）与双参数（兼容旧写法）都认。
			if !ok || len(call.Args) == 0 || len(call.Args) > 2 {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "tr" {
				return true
			}
			a, ok1 := call.Args[0].(*ast.BasicLit)
			if !ok1 || a.Kind != token.STRING {
				return true
			}
			en, err1 := strconv.Unquote(a.Value)
			if err1 != nil {
				return true
			}
			if _, seen := out[en]; !seen {
				out[en] = ""
			}
			if len(call.Args) == 2 {
				if b, ok2 := call.Args[1].(*ast.BasicLit); ok2 && b.Kind == token.STRING {
					if zh, err2 := strconv.Unquote(b.Value); err2 == nil {
						out[en] = zh
					}
				}
			}
			return true
		})
	}
	return out, nil
}
