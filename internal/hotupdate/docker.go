package hotupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// DockerClient 通过挂载的 /var/run/docker.sock 与 Docker Engine 通信。
//
// 为什么不用第三方 SDK：项目零依赖是硬约束，而 Docker API 就是普通 HTTP
// over unix socket，标准库 net.Dial 完全够用。
type DockerClient struct {
	socket string
	http   *http.Client
}

// NewDockerClient 创建客户端，默认 socket 路径 /var/run/docker.sock。
func NewDockerClient() *DockerClient {
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	return &DockerClient{
		socket: sock,
		// pull 大镜像可能持续数分钟，超时放宽；请求级另有 ctx 控制
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
			Timeout: 15 * time.Minute,
		},
	}
}

// dockerRound 对 Docker Engine 发起一次请求。
func (d *DockerClient) dockerRound(method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://localhost"+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Host", "docker")
	return d.http.Do(req)
}

// inContainer 判断当前进程是否运行在容器内。
// /.dockerenv 是 Docker/containerd 约定俗成的标记文件。
func inContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// dockerSocketAvailable 检查 docker.sock 是否已挂载进容器。
func dockerSocketAvailable() bool {
	_, err := os.Stat("/var/run/docker.sock")
	return err == nil
}

// ContainerInfo 是重建容器所需的自身信息。
type ContainerInfo struct {
	ID              string         `json:"Id"`
	Name            string         `json:"Name"`
	Image           string         `json:"Image"`
	Config          map[string]any `json:"Config"`
	HostConfig      map[string]any `json:"HostConfig"`
	NetworkSettings struct {
		Ports map[string]any `json:"Ports"`
	} `json:"NetworkSettings"`
}

// inspectSelf 通过容器 hostname（默认为容器 ID 短码）找到自己。
//
// 为什么不用 label：docker run 时用户未必打 label；hostname 默认就是
// 容器 ID 前 12 位，Docker API 支持 /containers/{id} 精确查找。
func (d *DockerClient) inspectSelf() (*ContainerInfo, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("读取 hostname 失败: %w", err)
	}
	resp, err := d.dockerRound("GET", "/v1.41/containers/"+host+"/json", nil, "")
	if err != nil {
		return nil, fmt.Errorf("docker.sock 不可达: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("inspect 容器失败: HTTP %d %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	var info ContainerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// PullImage 拉取指定镜像（Docker API 的 pull 是流式长请求，进度行按 \r\n 推送）。
// 完成返回镜像 ID（sha256:xxx）。
func (d *DockerClient) PullImage(ref string) (string, error) {
	resp, err := d.dockerRound("POST",
		"/v1.41/images/create?fromImage="+strings.ReplaceAll(ref, ":", "%3A"), nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 读完整流：Docker 会推送 JSON 进度行直到流结束
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("pull 失败: HTTP %d %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	// 逐行找 status 里带 digest 的最后一行
	var imageID string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m struct {
			Status string `json:"status"`
			ID     string `json:"id"`
		}
		if json.Unmarshal([]byte(line), &m) == nil {
			if strings.HasPrefix(m.Status, "Digest:") || strings.Contains(m.Status, "Downloaded newer image") {
				// 记录但真正的 ID 用 inspect 拿更稳
				continue
			}
		}
	}
	// inspect 拿镜像 ID
	resp2, err := d.dockerRound("GET", "/v1.41/images/"+ref+"/json", nil, "")
	if err != nil {
		return "", err
	}
	defer resp2.Body.Close()
	raw2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode == 200 {
		var im struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(raw2, &im) == nil {
			imageID = im.ID
		}
	}
	if imageID == "" {
		return "", fmt.Errorf("pull 后 inspect 不到镜像 %s", ref)
	}
	return imageID, nil
}

// truncateStr 截断长文本（本包私有副本，避免跨包导出工具函数）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// RecreateSelf 用官方新镜像重建当前容器。
//
// 核心难题：执行重建的进程就在旧容器里，一旦 stop 旧容器，
// 重建流程自己也死了（首版实现就死在这 —— 容器停在 Exited，
// rename/create/start 全没执行）。
//
// 解法（watchtower 同款）：先启动一个**独立 helper 容器**（docker:cli 镜像 +
// 动态生成的接管脚本），由它负责 stop 旧容器 → rename → 用原配置创建新容器
// → start → 清理。本进程只负责 pull 镜像、启动 helper、然后自行退出。
func (d *DockerClient) RecreateSelf(officialImage string) (string, error) {
	self, err := d.inspectSelf()
	if err != nil {
		return "", err
	}
	oldName := strings.TrimPrefix(self.Name, "/")
	oldImage := self.Image

	// 安全闸：只重建跑着我们官方镜像的容器。
	// 注意 inspect 的 Image 字段是运行时 digest（sha256:xxx），不含镜像名；
	// 创建时的引用在 Config.Image 里（如 ghcr.io/wangct233-source/wb2go:latest）。
	// 早前拿 Image 判断，官方容器也会被误判"非官方"，安全闸变成铁门。
	cfgImage, _ := self.Config["Image"].(string)
	isOfficial := strings.Contains(cfgImage, "wangct233-source/wb2go") ||
		strings.Contains(oldImage, "wangct233-source/wb2go")
	if !isOfficial {
		return "", fmt.Errorf("安全检查未通过：当前容器镜像 %s（%s）不是官方 wb2go 镜像，拒绝自动重建", cfgImage, truncateStr(oldImage, 19))
	}

	// 1. pull 新镜像并比对 ID
	newID, err := d.PullImage(officialImage)
	if err != nil {
		return "", err
	}
	curResp, err := d.dockerRound("GET", "/v1.41/images/"+oldImage+"/json", nil, "")
	if err == nil {
		var im struct {
			ID string `json:"Id"`
		}
		raw, _ := io.ReadAll(curResp.Body)
		curResp.Body.Close()
		if json.Unmarshal(raw, &im) == nil && im.ID == newID {
			return "已是最新版本，无需更新。", nil
		}
	}

	// 2. 生成 helper 接管脚本（参数从当前容器配置动态提取）
	script := buildHelperScript(self, officialImage, oldName)

	// 3. 启动 helper 容器（独立于旧容器生命周期，stop 旧容器杀不到它）
	helperBody := map[string]any{
		"Image": HelperImage,
		"Cmd":   []string{"sh", "-c", script},
		"HostConfig": map[string]any{
			"Binds":      []string{d.socket + ":/var/run/docker.sock"},
			"AutoRemove": true,
		},
	}
	buf, _ := json.Marshal(helperBody)
	resp, err := d.dockerRound("POST", "/v1.41/containers/create?name=wb2go-update-helper", strings.NewReader(string(buf)), "application/json")
	if err != nil {
		return "", fmt.Errorf("启动迁移 helper 失败: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("创建 helper 容器失败: HTTP %d %s", resp.StatusCode, truncateStr(string(raw), 200))
	}

	// 4. 给 helper 一点启动时间，然后本进程自行退出（stop 自己）。
	//    之后由 helper 完成 stop → rename → create → start → 清理。
	time.Sleep(3 * time.Second)
	d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/stop?t=3", nil, "")

	// 走到这里 normally 到不了（进程已随容器停止）。
	return fmt.Sprintf("迁移 helper 已接管：旧容器将停止并由镜像 %s 重建。", officialImage), nil
}

// HelperImage 是执行迁移的 helper 容器镜像（含 docker CLI）。
const HelperImage = "docker:27-cli"

// buildHelperScript 从当前容器配置生成接管脚本。
//
// 提取范围覆盖本项目所有部署用例：端口绑定、binds、env、restart 策略、
// 组附加、labels。够用且可审计 —— 不做通用容器迁移器。
func buildHelperScript(self *ContainerInfo, newImage, oldName string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("sleep 2\n")
	b.WriteString(fmt.Sprintf("docker stop -t 5 %s\n", oldName))
	b.WriteString(fmt.Sprintf("docker rename %s %s-old\n", oldName, oldName))

	// docker run 参数
	args := fmt.Sprintf("-d --name %s", oldName)
	hc := self.HostConfig
	if rp, ok := hc["RestartPolicy"].(map[string]any); ok {
		if name, ok := rp["Name"].(string); ok && name != "" {
			args += fmt.Sprintf(" --restart %s", name)
			if mv, ok := rp["MaximumRetryCount"].(float64); ok && mv > 0 {
				args += fmt.Sprintf(":%.0f", mv)
			}
		}
	}
	if binds, ok := hc["Binds"].([]any); ok {
		for _, v := range binds {
			if s, ok := v.(string); ok {
				args += fmt.Sprintf(" -v %q", s)
			}
		}
	}
	if pb, ok := hc["PortBindings"].(map[string]any); ok {
		for port, bindings := range pb {
			list, _ := bindings.([]any)
			for _, bind := range list {
				bm, _ := bind.(map[string]any)
				hostPort, _ := bm["HostPort"].(string)
				args += fmt.Sprintf(" -p %s:%s", hostPort, strings.TrimSuffix(port, "/tcp"))
			}
		}
	}
	if ga, ok := hc["GroupAdd"].([]any); ok {
		for _, v := range ga {
			if s, ok := v.(string); ok {
				args += " --group-add " + s
			} else if f, ok := v.(float64); ok {
				args += fmt.Sprintf(" --group-add %.0f", f)
			}
		}
	}
	if env, ok := self.Config["Env"].([]any); ok {
		for _, v := range env {
			if s, ok := v.(string); ok {
				args += fmt.Sprintf(" -e %q", s)
			}
		}
	}
	if labels, ok := self.Config["Labels"].(map[string]any); ok {
		for k, v := range labels {
			if s, ok := v.(string); ok && s != "" {
				args += fmt.Sprintf(" --label %q=%q", k, s)
			}
		}
	}
	args += " " + newImage
	b.WriteString("docker run " + args + "\n")
	b.WriteString(fmt.Sprintf("docker rm %s-old\n", oldName))
	b.WriteString("echo MIGRATION_DONE\n")
	return b.String()
}
