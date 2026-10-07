package upstream

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// Identity 是一套出站身份。
//
// 上游会按 UA、TLS 指纹、请求头族判断"这是不是官方客户端"。
// 三套身份对应官方三个真实产品的出站形态，等价于"官方客户端在说话"：
//
//	workbuddy  官方桌面客户端     → www.workbuddy.ai / copilot.tencent.com
//	vscode     官方 VSCode 插件   → www.workbuddy.ai / www.codebuddy.cn
//	cli        官方命令行客户端   → www.workbuddy.ai / copilot.tencent.com
type Identity struct {
	Key         string // workbuddy / vscode / cli
	Realm       string // cn / intl（由 Resolve 填入）
	DesktopVer  string // 桌面端版本号
	CLIVersion  string // CLI 版本号
	IdeType     string // X-IDE-Type 头值
	IdeName     string // X-IDE-Name 头值
	UserAgent   string // 出站 UA
	ChatBase    string // 聊天端点 base
	BillingBase string // 签到 / 积分等端点 base
	Origin      string // Origin / Referer 头值（由 Resolve 按区域填入）
	Product     string // X-Product 头值
}

// 构建三套身份。
//
// 版本号做成参数而非写死，原因：官方发版后写死的版本号会让网关"自报旧版本"，
// 这既是明显特征，也容易被版本闸门拦下。做成配置项后，至少能第一时间跟上。
func BuildIdentity(key, desktopVer, cliVer, product string) (Identity, error) {
	switch key {
	case "workbuddy":
		if desktopVer == "" {
			desktopVer = "5.5.6"
		}
		if cliVer == "" {
			cliVer = "2.137.1"
		}
		return Identity{
			Key: "workbuddy", DesktopVer: desktopVer, CLIVersion: cliVer,
			IdeType: "WorkBuddy", IdeName: "WorkBuddy",
			// 官方桌面端的 UA 是三段式：产品名重复两次 + CLI 版本
			UserAgent: fmt.Sprintf("WorkBuddy/%s WorkBuddy/%s CLI/%s", desktopVer, desktopVer, cliVer),
			Product:   productOr(product, "SaaS"),
		}, nil

	case "vscode":
		if desktopVer == "" {
			desktopVer = "4.9.29177644"
		}
		return Identity{
			Key: "vscode", DesktopVer: desktopVer, CLIVersion: cliVer,
			IdeType: "VSCode", IdeName: "VSCode",
			UserAgent: fmt.Sprintf("VSCode/1.119.0 WorkBuddy/%s", desktopVer),
			Product:   productOr(product, "SaaS"),
		}, nil

	case "cli":
		if cliVer == "" {
			cliVer = "2.137.1"
		}
		return Identity{
			Key: "cli", DesktopVer: desktopVer, CLIVersion: cliVer,
			IdeType: "CLI", IdeName: "CLI",
			UserAgent:   fmt.Sprintf("CLI/%s CodeBuddy/%s", cliVer, cliVer),
			ChatBase:    "https://www.workbuddy.ai",
			BillingBase: "https://www.workbuddy.ai",
			Product:     productOr(product, "CodeBuddy"),
		}, nil
	}
	return Identity{}, fmt.Errorf("未知出站身份 %q（可选 workbuddy / vscode / cli）", key)
}

func productOr(p, def string) string {
	if p != "" {
		return p
	}
	return def
}

// Resolve 把身份按区域绑定到实际端点。
//
// 这是区域隔离的唯一收口点：所有出站请求都要先过这里，
// 保证不会把国内账号的请求发到国际端点（会被拒，反之亦然）。
func (id Identity) Resolve(realm string) Identity {
	id.Realm = realm
	id.ChatBase = ChatBaseFor(realm)
	id.BillingBase = BillingBaseFor(realm)
	// 国内 VSCode 插件走 Web 域（workbuddy.cn）而非 CLI 域，这是官方行为
	if realm != "intl" && id.Key == "vscode" {
		id.ChatBase = CNWebBase
	}
	id.Origin = id.BillingBase
	return id
}

// 派生指纹标识。
//
// 关键设计：同一账号每次出站都来自"同一台虚拟设备"，不随机漂移；
// 不同账号彼此独立，阻断跨账号关联风控。随机 UUID 做不到这点——
// 每次请求一个新设备号在服务端看来是异常行为。
func derive(uid, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:])
}

// Fingerprint 是一个账号的稳定设备指纹。
type Fingerprint struct {
	MachineID    string
	SessionID    string
	RequestID    string
	DesktopVer   string
	CLIVersion   string
	IdeName      string
	IdeType      string
	Product      string
	OS           string
	Arch         string
	OSVersion    string
	CPUCores     int
	MemoryGB     int
	Timezone     string
	ReportDelay  int
	ReleaseEpoch int64
	Commit       string
}

// NewFingerprint 从账号 UID 派生一份稳定指纹。
//
// 硬件信息（CPU 核数、内存、系统版本）取一组固定的常见值：
// 它们必须"看起来像一台正常的开发机"，且在账号之间保持稳定。
// 随机化这些字段反而更可疑——真实机器的硬件不会每分钟变一次。
func NewFingerprint(uid string, id Identity) Fingerprint {
	if uid == "" {
		uid = "anonymous"
	}
	return Fingerprint{
		MachineID:   derive(uid, "machine"),
		SessionID:   derive(uid, "session"),
		RequestID:   derive(uid, "req") + "-" + strconv.FormatInt(nowMillis(), 10),
		DesktopVer:  id.DesktopVer,
		CLIVersion:  id.CLIVersion,
		IdeName:     id.IdeName,
		IdeType:     id.IdeType,
		Product:     id.Product,
		OS:          "win32",
		Arch:        "x64",
		OSVersion:   "10.0.26220",
		CPUCores:    20,
		MemoryGB:    24,
		Timezone:    "Asia/Shanghai",
		ReportDelay: 2000,
	}
}

// CacheKey 生成 prompt_cache_key。
//
// 必须带 uid8 作为硬隔离因子：上游缓存按账号隔离，
// 共享 key 会让一个账号命中另一个账号的前缀缓存——那等于把对话内容泄露出去。
func (f Fingerprint) CacheKey(sessionKey string) string {
	uid8 := f.MachineID
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	sum := sha256.Sum256([]byte(f.MachineID + "|" + sessionKey))
	return "wb2go-" + uid8 + "-" + hex.EncodeToString(sum[:16])
}

// stableRequestID 生成每次请求唯一但同源可追溯的请求 ID。
func stableRequestID(machineID string, seq int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", machineID, seq)))
	return hex.EncodeToString(sum[:16])
}

// redact 把敏感值打码，用于日志。
func redact(s string, keep int) string {
	if s == "" {
		return ""
	}
	if len(s) <= keep {
		return s[:1] + "***"
	}
	return s[:keep] + "***"
}

func nowMillis() int64 { return time.Now().UnixMilli() }
