package main

// 版本检测与升级（`hermes version --check` / `hermes upgrade`）。
//
// 为什么要它：用户装完之后，"我这是不是最新版"目前只能去网页看。而这是一个
// **自包含的二进制**，升级方式应当和安装方式一致 —— 一条命令。
//
// 红线：只用标准库（net/http、encoding/json、crypto/sha256），不引入任何第三方
// 依赖；下载的产物是我们自己的发布包，且**校验 SHA256** 之后才替换二进制。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const releaseRepo = "ChristopherWangDEV/Hermes"

// releaseInfo 是 GitHub Releases API 里我们用得到的字段。
type releaseInfo struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// platformAsset 返回当前平台的产物名，例如 hermes-1.2.1-darwin-arm64.tar.gz。
func platformAsset(version string) string {
	osName := "linux"
	if runtime.GOOS == "darwin" {
		osName = "darwin"
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	}
	return fmt.Sprintf("hermes-%s-%s-%s.tar.gz", version, osName, arch)
}

func httpGet(url string) ([]byte, error) {
	c := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	// GitHub API 要求 User-Agent；不给会 403，而 403 的信息量是零。
	req.Header.Set("User-Agent", "hermes/"+semver)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// latestRelease 查最新发布的 tag。
func latestRelease() (*releaseInfo, error) {
	body, err := httpGet("https://api.github.com/repos/" + releaseRepo + "/releases/latest")
	if err != nil {
		return nil, err
	}
	var rel releaseInfo
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("发布信息不是合法 JSON: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("发布信息里没有 tag_name")
	}
	return &rel, nil
}

// semverLess 比较两个 x.y.z。返回 true 表示 a < b。
// 只处理三段数字：这是本项目实际的版本形态，多写一套通用实现只会多一处出错的地方。
func semverLess(a, b string) bool {
	pa, pb := parseSemver(a), parseSemver(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseSemver(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	var out [3]int
	parts := strings.SplitN(v, ".", 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		n := 0
		for _, r := range parts[i] {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

// cmdVersionCheck 实现 `hermes version --check`。
func cmdVersionCheck() int {
	fmt.Printf("%s\n", fmt.Sprintf(tr("current: %s"), semver))
	rel, err := latestRelease()
	if err != nil {
		// 网络不通不是"出错"，是"查不到" —— 说清楚，别让用户以为版本坏了。
		fmt.Fprintf(os.Stderr, tr("cannot reach the release feed: %v\n"), err)
		return 4
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if semverLess(semver, latest) {
		fmt.Printf("%s\n", fmt.Sprintf(tr("latest:    %s  (update available: hermes upgrade)"), latest))
		return 0
	}
	fmt.Printf("%s\n", fmt.Sprintf(tr("latest:    %s  (up to date)"), latest))
	return 0
}

// cmdUpgrade 实现 `hermes upgrade`：下载 → 校验 → 原子替换。
func cmdUpgrade(args []string) int {
	checkOnly := false
	for _, a := range args {
		switch a {
		case "--check":
			checkOnly = true
		case "-h", "--help", "help":
			fmt.Print(upgradeUsage)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "hermes upgrade: %s %q\n\n%s", tr("unknown argument"), a, upgradeUsage)
			return 2
		}
	}
	if checkOnly {
		return cmdVersionCheck()
	}

	rel, err := latestRelease()
	if err != nil {
		fmt.Fprintf(os.Stderr, tr("cannot reach the release feed: %v\n"), err)
		return 4
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if !semverLess(semver, latest) {
		fmt.Printf("%s\n", fmt.Sprintf(tr("already up to date (%s)"), semver))
		return 0
	}

	asset := platformAsset(latest)
	var assetURL, sumsURL string
	for _, a := range rel.Assets {
		if a.Name == asset {
			assetURL = a.URL
		}
		if strings.HasPrefix(a.Name, "SHA256SUMS") {
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		fmt.Fprintf(os.Stderr, tr("this release has no %s (no build for this platform)\n"), asset)
		return 4
	}
	if sumsURL == "" {
		// 没有校验和就**不升级**：替换二进制前必须能验证来源。
		fmt.Fprintf(os.Stderr, tr("release has no SHA256SUMS; refusing to replace the binary\n"))
		return 4
	}

	fmt.Printf("%s\n", fmt.Sprintf(tr("downloading %s ..."), asset))
	sums, err := httpGet(sumsURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes upgrade: %v\n", err)
		return 4
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == asset {
			want = f[0]
			break
		}
	}
	if want == "" {
		fmt.Fprintf(os.Stderr, tr("SHA256SUMS has no entry for %s\n"), asset)
		return 4
	}
	blob, err := httpGet(assetURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes upgrade: %v\n", err)
		return 4
	}
	sum := sha256.Sum256(blob)
	got := hex.EncodeToString(sum[:])
	if got != want {
		fmt.Fprintf(os.Stderr, tr("checksum mismatch: got %s want %s\n"), got, want)
		return 4
	}
	fmt.Printf("%s\n", tr("checksum ok"))

	// 落盘到临时文件再原子替换：中途失败不能把正在用的二进制弄坏。
	dir := filepath.Dir(os.Args[0])
	tmp := filepath.Join(dir, ".hermes-upgrade.tar.gz")
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "hermes upgrade: %v\n", err)
		return 4
	}
	fmt.Printf("%s\n", fmt.Sprintf(tr("saved %s"), tmp))
	fmt.Printf("%s\n", tr("next: extract it and run ./install.sh (verified package)"))
	return 0
}

const upgradeUsage = `hermes upgrade - install the latest release

Usage:
  hermes version --check     ask the release feed whether this build is current
  hermes upgrade             download the latest package for this platform
  hermes upgrade --check     same as version --check

Notes:
  The package is verified against SHA256SUMS before anything is written.
  Nothing is replaced in place: extraction and install stay explicit.
`
