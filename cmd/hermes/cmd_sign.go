package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"hermes/internal/archive"
	"hermes/internal/sign"
)

// sign 子命令族：签名与验签。
//
// 密钥用 PEM 文件（PKCS#8 私钥 / 公钥），与 openssl 可直接互转 ——
// 用户的密钥可能来自别处的密钥管理流程，我们不该要求它只认一种格式。
func cmdSign(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, signUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "keygen":
		return cmdSignKeygen(rest)
	case "hbc":
		return cmdSignHBC(rest)
	case "verify":
		return cmdSignVerify(rest)
	case "-h", "--help", "help":
		fmt.Print(signUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "hermes sign: %s %q\n\n%s", tr("unknown subcommand"), sub, signUsage)
		return 2
	}
}

func cmdSignKeygen(args []string) int {
	var outPath string
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			outPath = args[i+1]
			i++
		}
	}
	if outPath == "" {
		outPath = "hermes.key"
	}
	k, err := sign.NewKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign keygen: %v\n"), err)
		return 4
	}
	der, err := x509.MarshalPKCS8PrivateKey(k.Priv)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign keygen: %v\n"), err)
		return 4
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	// 私钥文件权限 0600：这是唯一能把它变得"不是我们的问题"的做法。
	if err := os.WriteFile(outPath, privPEM, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign keygen: cannot write %s: %v\n"), outPath, err)
		return 4
	}
	pubDER, _ := x509.MarshalPKIXPublicKey(k.Pub)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(outPath+".pub", pubPEM, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign keygen: cannot write public key: %v\n"), err)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes sign keygen: private key %s (0600), public key %s.pub, key_id=%s\n"),
		outPath, outPath, k.ID())
	return 0
}

func loadKey(path string) (*sign.Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, fmt.Errorf(tr("%s is not PEM"), path)
	}
	priv, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := priv.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf(tr("%s is not an Ed25519 private key"), path)
	}
	return &sign.Key{Priv: ed, Pub: ed.Public().(ed25519.PublicKey)}, nil
}

func loadPub(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, fmt.Errorf(tr("%s is not PEM"), path)
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf(tr("%s is not an Ed25519 public key"), path)
	}
	return ed, nil
}

func cmdSignHBC(args []string) int {
	var hbc, keyPath, outPath string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-k", "--key":
			if i+1 < len(args) {
				keyPath = args[i+1]
				i++
			}
		case "-o", "--out":
			if i+1 < len(args) {
				outPath = args[i+1]
				i++
			}
		default:
			if hbc == "" && args[i] != "" && args[i][0] != '-' {
				hbc = args[i]
			}
		}
	}
	if hbc == "" || keyPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes sign hbc: need <file.hbc> -k <key.pem>\n\n")+signUsage)
		return 2
	}
	data, err := os.ReadFile(hbc)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign hbc: cannot read %s: %v\n"), hbc, err)
		return 4
	}
	if _, err := archive.Read(data); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign hbc: %s is not a usable archive: %v\n"), hbc, err)
		return 4
	}
	k, err := loadKey(keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign hbc: %v\n"), err)
		return 4
	}
	signed, err := sign.Sign(data, k)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign hbc: %v\n"), err)
		return 4
	}
	if outPath == "" {
		outPath = hbc
	}
	if err := os.WriteFile(outPath, signed, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign hbc: cannot write %s: %v\n"), outPath, err)
		return 4
	}
	fmt.Fprintf(os.Stderr, tr("hermes sign hbc: signed %s (key_id=%s)\n"), outPath, k.ID())
	return 0
}

func cmdSignVerify(args []string) int {
	var hbc, pubPath string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p", "--pub":
			if i+1 < len(args) {
				pubPath = args[i+1]
				i++
			}
		default:
			if hbc == "" && args[i] != "" && args[i][0] != '-' {
				hbc = args[i]
			}
		}
	}
	if hbc == "" || pubPath == "" {
		fmt.Fprint(os.Stderr, tr("hermes sign verify: need <file.hbc> -p <pub.pem>\n\n")+signUsage)
		return 2
	}
	data, err := os.ReadFile(hbc)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign verify: cannot read %s: %v\n"), hbc, err)
		return 4
	}
	pub, err := loadPub(pubPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign verify: %v\n"), err)
		return 4
	}
	if err := sign.VerifyWithPub(data, pub); err != nil {
		fmt.Fprintf(os.Stderr, tr("hermes sign verify: %v\n"), err)
		return 4
	}
	fmt.Printf("ok  %s  key_id=%s\n", hbc, sign.KeyID(data))
	return 0
}

var signUsage = pickUsage(`hermes sign - sign and verify .hbc (Ed25519)

Usage:
  hermes sign keygen [-o <key.pem>]                    generate a key pair (private 0600)
  hermes sign hbc <file.hbc> -k <key.pem> [-o <out>]   sign an archive
  hermes sign verify <file.hbc> -p <pub.pem>           verify a signature

Notes:
  The signed message is "hermes.hbc.v1\0" + the index bytes. It does NOT cover member
  bodies: hermes link must be able to reuse someone else archive without their private
  key, otherwise the whole distribution chain breaks. Member integrity comes from
  ir_sha256 in the index, and the index is what the key vouches for.

Exit codes:
  0  success
  2  usage error
  4  unreadable / not a usable archive / verification failed
`,
	`hermes sign —— .hbc 的签名与验签（Ed25519）

用法:
  hermes sign keygen [-o <key.pem>]              生成密钥对（私钥 0600，公钥 .pub）
  hermes sign hbc <file.hbc> -k <key.pem> [-o <out>]
  hermes sign verify <file.hbc> -p <pub.pem>

说明:
  签名消息 = "hermes.hbc.v1\0" + 索引字节。**不覆盖成员全文** ——
  否则 hermes link 复用别人的成员库时，必须用原私钥重签，整条分发链会断。
  成员完整性由索引里的 ir_sha256 保证，而索引本身由这把钥匙背书。

退出码:
  0  成功
  2  用法错误
  4  读不到 / 不是可用归档 / 验签失败
`)
