package guard_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 许可红线守卫（Rust 传递依赖）
//
// 为什么需要它：`native/rust/Cargo.toml` 的注释写着"第三方依赖仅 sha2 与
// zeroize"，但 **Cargo.lock 里有 11 个 crate** —— 另外 9 个是 sha2/zeroize
// 的传递依赖（digest、crypto-common、block-buffer、generic-array、typenum、
// cfg-if、cpufeatures、libc、version_check）。注释只登记了直接依赖，
// 于是"加一个依赖、顺带拖进来一个 GPL 的传递依赖"这件事没有任何机器会拦。
//
// 这正是本项目反复踩到的同一类缺口：**文档里写着的不算数，能被断言的才算数**。
//
// 本守卫做三件事：
//   ① Cargo.lock 里每个 crate 必须在 crateLicense 表里登记（漏登记即 fail）；
//   ② 登记的许可串按 OR / AND / "/" 拆开后，**每一段**都要在白名单里；
//   ③ crateLicense 表里不能有 Cargo.lock 里不存在的死条目（防止表自己腐烂）。
//
// 第 ② 条是整个守卫的核心，别把它弱化成"包含 MIT 就通过"：
// Rust crate 的 license 字段允许 "GPL-3.0 OR MIT" 这种写法，
// 子串匹配会让这种 crate 大摇大摆地通过。
// ---------------------------------------------------------------------------

// 许可白名单。与 PRD §8.1 / DECISIONS D-025 红线一致：
// MIT / Apache-2.0 / BSD / ISC / 0BSD / Zlib，以及 LLVM 例外。
// 不含任何 GPL 家族（含带运行时库例外的）、MPL、EUPL、SSPL、BUSL、CC-BY-NC。
var allowedLicenseAtoms = map[string]bool{
	"MIT":                            true,
	"Apache-2.0":                     true,
	"Apache-2.0 WITH LLVM-exception": true,
	"BSD-2-Clause":                   true,
	"BSD-3-Clause":                   true,
	"ISC":                            true,
	"0BSD":                           true,
	"Zlib":                           true,
}

// crateLicense 是本机 ~/.cargo/registry 实测抄录的事实表（2026-11-03）。
// Cargo.lock 本身不含 license 字段，所以这张表就是唯一事实源 ——
// 加任何依赖都必须同步更新它，否则 ① 会立刻红。
var crateLicense = map[string]string{
	// 直接依赖（Cargo.toml 里显式声明的两个）
	"sha2":    "MIT OR Apache-2.0",
	"zeroize": "Apache-2.0 OR MIT",
	// 传递依赖（sha2 侧：digest → crypto-common → block-buffer → generic-array → typenum）
	"digest":        "MIT OR Apache-2.0",
	"crypto-common": "MIT OR Apache-2.0",
	"block-buffer":  "MIT OR Apache-2.0",
	"generic-array": "MIT",
	"typenum":       "MIT OR Apache-2.0",
	"cfg-if":        "MIT OR Apache-2.0",
	"cpufeatures":   "MIT OR Apache-2.0",
	// 传递依赖（zeroize / std 侧）
	"libc":          "MIT OR Apache-2.0",
	"version_check": "MIT/Apache-2.0", // 注意分隔符是 "/" 不是 " OR "
}

// 本 crate 自己：跟随仓库根的 Apache-2.0，不参与白名单比对。
const selfCrate = "hermes-core"

// licenseAtoms 把一个 SPDX 许可表达式拆成原子许可标识符。
// 分隔符是 OR / AND / "/"，大小写不敏感。
// 不能拆 "WITH" —— 它是 "Apache-2.0 WITH LLVM-exception" 的一部分。
func licenseAtoms(expr string) []string {
	re := regexp.MustCompile(`(?i)\s*(?:OR|AND|/)\s*`)
	var out []string
	for _, p := range re.Split(expr, -1) {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func TestRustDependencyLicenses(t *testing.T) {
	root := repoRoot(t)

	lockPath := filepath.Join(root, "native", "rust", "Cargo.lock")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("读不到 %s：%v（Rust 依赖树是红线的一部分，缺文件不能跳过）", lockPath, err)
	}

	// Cargo.lock 里 "name = ..." 只出现在 [[package]] 块内；
	// dependencies 列表是裸字符串（"sha2",），不带 name =，不会误命中。
	nameRe := regexp.MustCompile(`(?m)^name = "([^"]+)"$`)
	seen := map[string]bool{}
	for _, m := range nameRe.FindAllStringSubmatch(string(raw), -1) {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		t.Fatalf("%s 里解析出 0 个 package —— 守卫空转，等于没有守卫", lockPath)
	}
	delete(seen, selfCrate)

	// ① 每个 crate 都必须登记
	var missing []string
	for name := range seen {
		if _, ok := crateLicense[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("Cargo.lock 里有 %d 个 crate 未在许可表登记（加依赖必须同步更新 crateLicense）：%v",
			len(missing), missing)
	}

	// ③ 表里不能有死条目
	var stale []string
	for name := range crateLicense {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		t.Errorf("许可表里有 %d 个 Cargo.lock 中不存在的死条目（依赖已移除就该删登记）：%v",
			len(stale), stale)
	}

	// ② 每段许可都要在白名单里
	for name, expr := range crateLicense {
		atoms := licenseAtoms(expr)
		if len(atoms) == 0 {
			t.Errorf("%s 的许可串 %q 拆不出任何原子许可", name, expr)
			continue
		}
		for _, a := range atoms {
			if !allowedLicenseAtoms[a] {
				t.Errorf("%s 的许可 %q 不在白名单（红线：禁 GPL 家族 / MPL / EUPL / SSPL / BUSL / CC-BY-NC）",
					name, a)
			}
		}
	}
}

// TestRustLicenseTableMatchesRegistry 是交叉验证，防止 crateLicense 表自己漂移：
// 本机有 ~/.cargo/registry 时，逐条比对表里写的与实际下载的 crate 的 license 字段。
// 没有 registry（CI 干净环境）时跳过交叉验证 —— 但上一 Test 的主断言照跑，不依赖它。
func TestRustLicenseTableMatchesRegistry(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("拿不到 HOME：%v", err)
	}
	regRoot := filepath.Join(home, ".cargo", "registry", "src")
	entries, err := os.ReadDir(regRoot)
	if err != nil {
		t.Skipf("本机没有 %s，跳过交叉验证（主断言已在上一 Test 跑过）", regRoot)
	}

	licRe := regexp.MustCompile(`(?m)^license\s*=\s*"([^"]*)"`)
	checked := 0
	for name, want := range crateLicense {
		found := ""
		for _, reg := range entries {
			matches, err := filepath.Glob(filepath.Join(regRoot, reg.Name(), name+"-*"))
			if err != nil || len(matches) == 0 {
				continue
			}
			for _, dir := range matches {
				b, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
				if err != nil {
					continue
				}
				if m := licRe.FindStringSubmatch(string(b)); m != nil {
					found = m[1]
					break
				}
			}
			if found != "" {
				break
			}
		}
		if found == "" {
			t.Logf("%s 在本机 registry 中未找到，跳过", name)
			continue
		}
		checked++
		if !sameLicenseSet(found, want) {
			t.Errorf("%s 许可已变化：本机 registry 是 %q，表里写的是 %q（请同步 crateLicense）",
				name, found, want)
		}
	}
	if checked == 0 {
		t.Logf("registry 里一个都没匹配上，交叉验证未生效（不影响主断言）")
	}
}

// sameLicenseSet 比许可表达式，忽略分隔符与原子顺序（"MIT OR Apache-2.0" 与
// "Apache-2.0 OR MIT" 是同一件事）。
func sameLicenseSet(a, b string) bool {
	ma := map[string]bool{}
	for _, x := range licenseAtoms(a) {
		ma[x] = true
	}
	mb := map[string]bool{}
	for _, x := range licenseAtoms(b) {
		mb[x] = true
	}
	if len(ma) != len(mb) {
		return false
	}
	for k := range ma {
		if !mb[k] {
			return false
		}
	}
	return true
}
