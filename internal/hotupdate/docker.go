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
	// 真正的 ID 用 inspect 拿更稳（流式行里只有 digest）
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
		if json.Unmarshal(raw2, &im) == nil && im.ID != "" {
			return im.ID, nil
		}
	}
	return "", fmt.Errorf("pull 后 inspect 不到镜像 %s", ref)
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
// 重建流程自己也死了（首版实现就死在这 —— 容器停在 Exited）。
// 第二版用 docker:cli 做 helper，但服务器普遍拉不动 Docker Hub。
//
// 最终方案（零额外下载）：pull 新镜像后，用**新镜像自身**跑一个
// `wb2go migrate` 短命容器接管迁移 —— 新镜像里有迁移代码，docker.sock
// 挂进去即可操作 Docker API。本进程只负责 pull、起 helper、然后退出。
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

	// 2. 启动 migrate helper：跑刚 pull 的新镜像（内含最新迁移代码），
	//    挂 docker.sock，环境变量把旧容器名 / 新镜像名传给 migrate 子命令。
	helperBody := map[string]any{
		"Image": officialImage,
		"Cmd":   []string{"/app/wb2go", "migrate"},
		"Env": []string{
			"MIGRATE_OLD_NAME=" + oldName,
			"MIGRATE_NEW_IMAGE=" + officialImage,
			"TZ=Asia/Shanghai",
		},
		"HostConfig": map[string]any{
			"Binds":      []string{d.socket + ":/var/run/docker.sock"},
			"AutoRemove": true,
		},
	}
	buf, _ := json.Marshal(helperBody)
	resp, err := d.dockerRound("POST", "/v1.41/containers/create?name=wb2go-migrate-helper", strings.NewReader(string(buf)), "application/json")
	if err != nil {
		return "", fmt.Errorf("启动迁移 helper 失败: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("创建 helper 容器失败: HTTP %d %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	if resp, err := d.dockerRound("POST", "/v1.41/containers/wb2go-migrate-helper/start", nil, ""); err != nil {
		return "", fmt.Errorf("启动 helper 失败: %w", err)
	} else {
		resp.Body.Close()
	}

	// 3. 给 helper 起动时间（它 sleep 2 后才动旧容器），本进程自行退出。
	//    本进程退出 → 旧容器停止 → helper 接管 rename/create/start/rm。
	time.Sleep(3 * time.Second)
	d.dockerRound("POST", "/v1.41/containers/"+self.ID+"/stop?t=3", nil, "")

	return "迁移 helper 已接管，旧容器将由新镜像重建。", nil
}

// RunMigration 是 migrate 子命令的实现：接管旧容器的升级收尾。
//
// 流程：inspect 旧容器（宿主机传进来的名字）→ stop → rename 腾名 →
// 用原配置 + 新镜像创建同名词容器 → start → 删除 backup。
// helper 容器 AutoRemove，跑完即消失。
func RunMigration(oldName, newImage string) error {
	d := NewDockerClient()

	old, err := d.inspectByName(oldName)
	if err != nil {
		return fmt.Errorf("inspect 旧容器失败: %w", err)
	}

	// stop 旧容器（此时旧容器可能已在停止状态，ignore 错误）
	if resp, err := d.dockerRound("POST", "/v1.41/containers/"+old.ID+"/stop?t=5", nil, ""); err == nil {
		resp.Body.Close()
	}

	// rename 腾出名字
	backup := oldName + "-old"
	if resp, err := d.dockerRound("POST", "/v1.41/containers/"+old.ID+"/rename?name="+backup, nil, ""); err != nil {
		return fmt.Errorf("rename 失败: %w", err)
	} else {
		resp.Body.Close()
	}

	// 用原配置 + 新镜像创建同名容器
	createBody := map[string]any{
		"Image":      newImage,
		"Config":     old.Config,
		"HostConfig": old.HostConfig,
	}
	buf, _ := json.Marshal(createBody)
	resp, err := d.dockerRound("POST", "/v1.41/containers/create?name="+oldName, strings.NewReader(string(buf)), "application/json")
	if err != nil {
		// 回滚：把旧容器名字改回去并启动
		d.dockerRound("POST", "/v1.41/containers/"+old.ID+"/rename?name="+oldName, nil, "")
		d.dockerRound("POST", "/v1.41/containers/"+old.ID+"/start", nil, "")
		return fmt.Errorf("创建新容器失败（已回滚）: %w", err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if json.Unmarshal(raw, &created) != nil || created.ID == "" {
		d.dockerRound("POST", "/v1.41/containers/"+old.ID+"/rename?name="+oldName, nil, "")
		d.dockerRound("POST", "/v1.41/containers/"+old.ID+"/start", nil, "")
		return fmt.Errorf("创建响应异常（已回滚）: %s", truncateStr(string(raw), 200))
	}

	// 启动新容器
	if resp, err := d.dockerRound("POST", "/v1.41/containers/"+oldName+"/start", nil, ""); err != nil {
		return fmt.Errorf("新容器启动失败（旧容器保留为 %s）: %w", backup, err)
	} else {
		resp.Body.Close()
	}

	// 删除 backup（失败不影响）
	d.dockerRound("DELETE", "/v1.41/containers/"+old.ID+"?force=true&v=true", nil, "")
	fmt.Println("MIGRATION_DONE")
	return nil
}

// inspectByName 按容器名 inspect（migrate 子命令用，此时旧容器名已知）。
func (d *DockerClient) inspectByName(name string) (*ContainerInfo, error) {
	resp, err := d.dockerRound("GET", "/v1.41/containers/"+name+"/json", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	var info ContainerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
