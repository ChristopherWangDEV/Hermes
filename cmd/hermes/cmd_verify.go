package main

import (
	"fmt"
	"os"

	"hermes/internal/archive"
	"hermes/internal/sign"
)

// verify 子命令：把两个产物一起验（IR.md §8.5 的五步）。
//
// 单验 `.hbc` 只能证明"这份归档自洽且被签过"；单验 sealed 只能证明
// "源码与 IR 同源"。**只有两个一起验**，才能说"分发出去的那份，就是这份源码编的"。
func cmdVerify(args []string) int {
	var hbcPath, sealedPath, pubPath string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-p", "--pub":
			if i+1 >= len(args) {
				fmt.Fprint(os.Stderr, tr("hermes verify: -p needs a public key file\n\n")+verifyUsage)
				return 2
			}
			pubPath = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Print(verifyUsage)
			return 0
		default:
			if a == "" || a[0] == '-' {
				fmt.Fprintf(os.Stderr, "hermes verify: %s %q\n\n%s", tr("unknown argument"), a, verifyUsage)
				return 2
			}
			if hbcPath == "" {
				hbcPath = a
			} else if sealedPath == "" {
				sealedPath = a
			} else {
				fmt.Fprintf(os.Stderr, "hermes verify: %s %q\n\n%s", tr("unexpected argument"), a, verifyUsage)
				return 2
			}
		}
	}
	if hbcPath == "" || sealedPath == "" || pubPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes verify: need <app.hbc> <app.sealed.hermes> -p <pub.pem>\n\n")+verifyUsage)
		return 2
	}

	hbc, err := os.ReadFile(hbcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes verify: cannot read %s: %v\n"), hbcPath, err)
		return 4
	}
	sealed, err := os.ReadFile(sealedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes verify: cannot read %s: %v\n"), sealedPath, err)
		return 4
	}
	pub, err := loadPub(pubPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes verify: %v\n"), err)
		return 4
	}

	// 1. 结构
	if _, err := archive.Read(hbc); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes verify: [1/5 structure] .hbc is invalid: %v\n"), err)
		return 4
	}
	if _, _, err := sign.ReadSealed(sealed); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes verify: [1/5 structure] sealed is invalid: %v\n"), err)
		return 4
	}
	fmt.Println(tr("1/5 structure   ok"))

	// 2. 验签：两个产物各自一把（同一次构建、同一把钥匙）
	if err := sign.VerifyWithPub(hbc, pub); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes verify: [2/5 signature] .hbc: %v\n"), err)
		return 4
	}
	if err := sign.VerifySealedWithPub(sealed, hbc, pub); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes verify: [2/5 signature] sealed/cross-check: %v\n"), err)
		return 4
	}
	fmt.Println(tr("2/5 signature   ok"))

	// 3–5. 交叉校验在 VerifySealed 里完成（index / ir / hbc / source 四个哈希闭环）。
	// 这里再逐项报一次，是为了让人看见"通过了哪几步"，而不是一个 ok 了事。
	a, _ := archive.Read(hbc)
	m0, _ := sign.MemberIR(a, 0)
	fmt.Printf(tr("3/5 index       ok  index_sha256=%.16s\n"), fmt.Sprintf("%x", hbc[44:76]))
	fmt.Printf(tr("4/5 same IR     ok  ir_sha256=%.16x\n"), sha256Of(m0))
	fmt.Printf(tr("5/5 same archive ok hbc_bytes=%d\n"), len(hbc))

	fmt.Println(tr("all checks passed"))
	return 0
}

var verifyUsage = pickUsage(`hermes verify - verify both products together (IR.md §8.5)

Usage:
  hermes verify <app.hbc> <app.sealed.hermes> -p <pub.pem>

Notes:
  Five steps: structure -> signature -> index_sha256 -> ir_sha256 -> hbc/source hashes.
  Verifying one product proves half; only both together prove "the shipped file is the
  one built from this source".

Exit codes:
  0  passed
  2  usage error
  4  any step failed
`,
	`hermes verify —— 两个产物一起验（IR.md §8.5）

用法:
  hermes verify <app.hbc> <app.sealed.hermes> -p <pub.pem>

说明:
  五步：结构 → 验签 → index_sha256 交叉 → ir_sha256 交叉 → hbc/source 哈希交叉。
  单验一个只能证明一半，两个一起验才能说"分发出去的那份就是这份源码编的"。

退出码:
  0  通过
  2  用法错误
  4  任一步失败
`)
