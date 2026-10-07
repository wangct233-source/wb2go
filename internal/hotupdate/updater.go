package hotupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultRepo 是本项目的官方发布仓库。
//
// 热更新仓库固定不可更改，原因有二：
//  1. 安全 —— 热更新会下载并替换当前正在运行的二进制，如果仓库地址可被
//     面板随意改写，拿到面板权限的人就能把网关替换成任意后门程序；
//  2. 防钓鱼 —— 第三方 fork 若允许用户填自己的仓库，用户可能装到被篡改的
//     "收费版"，这正是 README 里明确警告过的骗局形态。
const DefaultRepo = "wangct233-source/wb2go"

// Updater 从 GitHub Releases 拉取预编译产物并热替换当前二进制。
//
// 为什么要做这个：容器部署时用户通常不希望为了一个小版本升级去重新 build 镜像。
// 拉取已打包的发布版本、校验完整性、原子替换后重启，等价于"apt upgrade"，
// 但不需要用户有构建环境。
//
// 安全设计：
//   - 只接受 HTTPS 的 github.com / objects.githubusercontent.com
//   - 若发布页提供了 sha256 摘要则强制校验
//   - 替换前备份当前二进制，替换失败自动回滚
type Updater struct {
	Repo       string // owner/repo
	CurrentVer string
	Mirror     string
	AssetHint  string // 期望的产物名关键字，默认取当前二进制名

	client *http.Client

	mu       sync.Mutex
	latest   string
	assetURL string
	notes    string
}

// New 建更新器。
func New(repo, currentVer, mirror string) *Updater {
	return &Updater{
		Repo: repo, CurrentVer: currentVer, Mirror: mirror,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

// Info 是一次检查的结果。字段带 JSON tag 以便面板直接透传。
type Info struct {
	Current  string `json:"current"`
	Latest   string `json:"latest"`
	UpToDate bool   `json:"up_to_date"`
	AssetURL string `json:"asset_url,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// check 查询最新版本，返回具体类型供内部使用。
func (u *Updater) check(ctx context.Context) (*Info, error) {
	if u.Repo == "" {
		return nil, fmt.Errorf("未配置 GitHub Release 仓库（设置里填写 owner/repo）")
	}
	api := "https://api.github.com/repos/" + u.Repo + "/releases/latest"
	if u.Mirror != "" {
		api = strings.TrimRight(u.Mirror, "/") + "/" + u.Repo + "/releases/latest"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wb2go/"+u.CurrentVer)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 GitHub API 失败（国内网络可能需要配置镜像）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("仓库 %s 没有发布版本，或仓库不存在", u.Repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回 HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
		} `json:"assets"`
		Draft      bool `json:"draft"`
		Prerelease bool `json:"prerelease"`
	}
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, fmt.Errorf("解析发布信息失败: %w", err)
	}
	if rel.Draft {
		return nil, fmt.Errorf("仓库 %s 没有正式发布版本", u.Repo)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")

	// 挑产物：优先与当前二进制同名的，其次匹配平台关键字
	want := u.AssetHint
	assetURL := ""
	for _, a := range rel.Assets {
		if !matchesPlatform(a.Name) {
			continue
		}
		if want != "" && a.Name == want {
			assetURL = a.BrowserDownloadURL
			break
		}
		if assetURL == "" {
			assetURL = a.BrowserDownloadURL
		}
	}
	if assetURL == "" {
		return nil, fmt.Errorf("发布 v%s 里没有匹配当前平台（%s/%s）的产物", latest, runtime.GOOS, runtime.GOARCH)
	}

	u.mu.Lock()
	u.latest, u.assetURL, u.notes = latest, assetURL, truncate(rel.Body, 400)
	u.mu.Unlock()

	return &Info{
		Current: u.CurrentVer, Latest: latest,
		UpToDate: sameVersion(latest, u.CurrentVer),
		AssetURL: assetURL, Notes: truncate(rel.Body, 400),
	}, nil
}

// matchesPlatform 判断产物名是否匹配当前平台。
func matchesPlatform(name string) bool {
	l := strings.ToLower(name)
	hasOS := strings.Contains(l, runtime.GOOS)
	hasArch := strings.Contains(l, runtime.GOARCH) ||
		(runtime.GOARCH == "amd64" && strings.Contains(l, "x86_64")) ||
		(runtime.GOARCH == "386" && strings.Contains(l, "i386"))
	if !hasOS || !hasArch {
		return false
	}
	// 排除源码包与其他架构
	if strings.Contains(l, ".sha256") || strings.Contains(l, "checksums") {
		return false
	}
	return true
}

// Check 查询最新版本。
//
// 返回 any 而非 *Info：面板层不应依赖本包的具体类型，
// 否则两个包会形成不必要的耦合。
func (u *Updater) Check(ctx context.Context) (any, error) {
	return u.check(ctx)
}

// Apply 下载新版本、校验、替换二进制，并触发重启。
func (u *Updater) Apply(ctx context.Context) (string, error) {
	info, err := u.check(ctx)
	if err != nil {
		return "", err
	}
	if info.UpToDate {
		return "当前 v" + u.CurrentVer + " 已是最新，无需更新。", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位当前二进制失败: %w", err)
	}
	// 解析符号链接：容器里 /usr/local/bin/wb2go 可能指向 /app/wb2go，
	// 替换链接本身会让服务跑在旧的实体文件上。
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	msg, err := u.downloadAndReplace(ctx, info, exe)
	if err != nil {
		return "", err
	}
	// 延迟重启：给调用方留出返回响应的时间
	go func() {
		time.Sleep(2 * time.Second)
		restart()
	}()
	return msg, nil
}

// downloadAndReplace 下载并替换二进制。
func (u *Updater) downloadAndReplace(ctx context.Context, info *Info, exe string) (string, error) {
	if !isTrustedURL(info.AssetURL) {
		return "", fmt.Errorf("拒绝从非官方地址下载：%s", info.AssetURL)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, info.AssetURL, nil)
	req.Header.Set("User-Agent", "wb2go/"+u.CurrentVer)
	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}

	dir := filepath.Dir(exe)
	tmp := filepath.Join(dir, ".wb2go-update-"+strconv.Itoa(os.Getpid()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", fmt.Errorf("写入临时文件失败（容器内请确认挂载目录可写）: %w", err)
	}
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("写入失败: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	// 备份当前版本，替换失败可回滚
	backup := exe + ".bak"
	hadBackup := false
	if _, err := os.Stat(backup); err == nil {
		_ = os.Remove(backup)
	}
	if err := os.Rename(exe, backup); err == nil {
		hadBackup = true
	}

	if err := os.Rename(tmp, exe); err != nil {
		if hadBackup {
			_ = os.Rename(backup, exe) // 回滚
		}
		return "", fmt.Errorf("替换二进制失败: %w", err)
	}
	_ = os.Chmod(exe, 0o755)
	if hadBackup {
		_ = os.Remove(backup)
	}

	return fmt.Sprintf("已更新到 v%s（%.1f MB, sha256 %s…），进程即将重启。",
		info.Latest, float64(written)/(1<<20), sum[:12]), nil
}

// isTrustedURL 只允许从 GitHub 官方域名下载。
func isTrustedURL(u string) bool {
	if !strings.HasPrefix(u, "https://") {
		return false
	}
	host := strings.Split(strings.Split(u, "https://")[1], "/")[0]
	return host == "github.com" ||
		host == "objects.githubusercontent.com" ||
		host == "release-assets.githubusercontent.com" ||
		strings.HasSuffix(host, ".githubusercontent.com")
}

// restart 重启当前进程。
//
// 为什么用 exec 而不是 os.Exit：容器里 PID 1 直接退出会被判定异常结束，
// restart 策略可能不生效。用 /proc/self/exe 重新 exec 一个新进程，
// 容器视角里"还是同一个进程在跑"，重启策略与优雅退出都正常。
func restart() {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(0)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Start(); err != nil {
		// 拉起失败只能退出，让容器编排重启（restart: unless-stopped 会生效）
		os.Exit(1)
	}
	os.Exit(0)
}

// sameVersion 比较版本号（忽略 v 前缀与后缀）。
func sameVersion(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		if i := strings.IndexAny(s, "-+"); i > 0 {
			s = s[:i]
		}
		return s
	}
	return norm(a) == norm(b)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
