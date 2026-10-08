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
// 这是"自杀式"操作，步骤必须严格：
//  1. inspect 自己，拿到原始 Name / Config / HostConfig
//  2. 校验当前容器镜像确实是我们官方镜像（防止误重建用户的其他容器）
//  3. stop 旧容器 → rename 成 {name}-old（腾出名字）→ 用原配置创建新容器 → start
//  4. 删除 backup。任何一步失败 → rename 回来，原容器恢复运行
//
// 返回给调用方时本进程可能已被杀掉（新容器顶替），所以调用方在
// 发起重建前就该把响应写给用户。
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
		return "", fmt.Errorf("安全检查未通过：当前容器镜像 %s（%s）不是官方 wb2go 镜像，拒绝自动重建", cfgImage, oldImage[:19])
	}

	// 新镜像 ID
	newID, err := d.PullImage(officialImage)
	if err != nil {
		return "", err
	}
	// 比对：当前容器用的镜像 ID 与新 pull 的一致 → 无需重建
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

	// 拷贝原始创建参数（Config/HostConfig 原样传回 Docker API）
	createBody := map[string]any{
		"Image":      officialImage,
		"Config":     self.Config,
		"HostConfig": self.HostConfig,
	}
	buf, _ := json.Marshal(createBody)

	// 1. 停旧
	if resp, err := d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/stop?t=10", nil, ""); err != nil {
		return "", fmt.Errorf("停止旧容器失败: %w", err)
	} else {
		resp.Body.Close()
	}

	// 2. rename 腾名
	backupName := oldName + "-update-backup"
	if resp, err := d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/rename?name="+backupName, nil, ""); err != nil {
		// 恢复启动
		d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/start", nil, "")
		return "", fmt.Errorf("重命名旧容器失败: %w", err)
	} else {
		resp.Body.Close()
	}

	// 3. 创建新容器（原名字、原配置、新镜像）
	if resp, err := d.dockerRound("POST", "/v1.41/containers/create?name="+oldName, strings.NewReader(string(buf)), "application/json"); err != nil {
		d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/rename?name="+oldName, nil, "")
		d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/start", nil, "")
		return "", fmt.Errorf("创建新容器失败: %w", err)
	} else {
		var created struct {
			ID string `json:"Id"`
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if json.Unmarshal(raw, &created) != nil || created.ID == "" {
			d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/rename?name="+oldName, nil, "")
			d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/start", nil, "")
			return "", fmt.Errorf("创建新容器响应异常: %s", truncateStr(string(raw), 200))
		}
	}

	// 4. 起新容器
	if resp, err := d.dockerRound("POST", "/v1.41/containers/"+oldName+"/start", nil, ""); err != nil {
		return "", fmt.Errorf("新容器启动失败（旧容器保留为 %s，可手动恢复）: %w", backupName, err)
	} else {
		resp.Body.Close()
	}

	// 5. 删备份（失败不影响，留给用户清理）
	d.dockerRound("DELETE", "/v1.41/containers/"+self.ID+"?force=true&v=true", nil, "")

	return fmt.Sprintf("容器已用新镜像重建（%s → %s），本进程即将被替换。", oldImage[:19], newID[:19]), nil
}
