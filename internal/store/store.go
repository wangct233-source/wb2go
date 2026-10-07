package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wb2go/wb2go/internal/config"
)

// Credentials 是一份账号凭证。字段名与 JSON key 一一对应，便于手工排查。
//
// 安全说明：Token 是明文保存的（上游没有可用的加密分发方式）。
// 因此文件权限强制 0600、目录 0700，且默认在 .gitignore 中排除。
type Credentials struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	Realm        string `json:"realm"` // cn | intl
	Identity     string `json:"identity"`
	EnterpriseID string `json:"enterpriseId,omitempty"`

	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // Unix 秒；0 = 未知
	AddedAt      int64  `json:"addedAt"`

	// 绑定的出站代理槽（留空 = 直连）。账号绑槽后其全部出站请求走该出口，
	// 避免"身份请求从宿主 IP 发出，把账号与宿主 IP 关联起来"。
	ProxySlot string `json:"proxySlot,omitempty"`
	ProxyURL  string `json:"proxy,omitempty"`

	Source string `json:"source,omitempty"` // 记录来源：oauth / import / manual
}

// Valid 判断凭证是否可用于请求。
func (c Credentials) Valid() bool {
	return c.UID != "" && c.AccessToken != ""
}

// NeedRefresh 判断是否需要在发请求前刷新 token。
// 提前 120 秒是为了覆盖"刷新花了几秒 + 请求路上花了几秒"的边界情况。
func (c Credentials) NeedRefresh(now int64) bool {
	if c.RefreshToken == "" {
		return false
	}
	if c.ExpiresAt == 0 {
		return true // 未知过期时间：保守刷新一次
	}
	return c.ExpiresAt-now < 120
}

// Display 返回面板上展示用的短名，避免把完整 UID 暴露在列表里。
func (c Credentials) Display() string {
	if c.Nickname != "" {
		return c.Nickname
	}
	if len(c.UID) > 8 {
		return c.UID[:8]
	}
	return c.UID
}

// Store 管理 accounts 目录下的一批凭证。
type Store struct {
	mu    sync.RWMutex
	dir   string
	cache map[string]*Credentials // uid -> 凭证
}

// uidPattern 是允许出现在文件名里的字符集。
//
// 这是一道路径穿越防线：上游返回的 uid 会被拼进文件路径，
// 若不做白名单校验，形如 "../../etc/passwd" 的 uid 就能写到目录之外。
var uidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidUID 校验 uid 是否可以安全用作文件名。
func ValidUID(uid string) bool { return uidPattern.MatchString(uid) }

func New(dir string) (*Store, error) {
	s := &Store{dir: dir, cache: map[string]*Credentials{}}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建账号目录 %s 失败: %w", dir, err)
	}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload 从磁盘重读全部凭证。启动时与面板"重新扫描"时调用。
func (s *Store) Reload() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("读取账号目录失败: %w", err)
	}
	fresh := make(map[string]*Credentials, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // 单个文件读失败不影响其他账号
		}
		var cred Credentials
		if err := json.Unmarshal(raw, &cred); err != nil {
			continue // 文件损坏时跳过，而不是让整个网关起不来
		}
		if !cred.Valid() {
			continue
		}
		fresh[cred.UID] = &cred
	}
	s.mu.Lock()
	s.cache = fresh
	s.mu.Unlock()
	return nil
}

// List 返回全部凭证的副本（按 UID 排序，保证面板展示稳定）。
func (s *Store) List() []Credentials {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Credentials, 0, len(s.cache))
	for _, c := range s.cache {
		out = append(out, *c)
	}
	sortByUID(out)
	return out
}

func sortByUID(list []Credentials) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].UID < list[j-1].UID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// Get 按 UID 取凭证副本。
func (s *Store) Get(uid string) (Credentials, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cache[uid]
	if !ok {
		return Credentials{}, false
	}
	return *c, true
}

// Save 落盘一份凭证并更新内存缓存。同 UID 覆盖。
func (s *Store) Save(c Credentials) error {
	if !ValidUID(c.UID) {
		// 这是路径穿越防线：uid 直接参与拼路径，必须先过白名单。
		return fmt.Errorf("uid 含非法字符，已拒绝写入（只允许字母数字下划线连字符，≤64）")
	}
	if c.AddedAt == 0 {
		c.AddedAt = time.Now().Unix()
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(s.path(c.UID), append(data, '\n'), 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	cp := c
	s.cache[c.UID] = &cp
	s.mu.Unlock()
	return nil
}

// Remove 删除一个账号的凭证文件。
func (s *Store) Remove(uid string) error {
	if !ValidUID(uid) {
		return fmt.Errorf("uid 含非法字符，已拒绝操作")
	}
	s.mu.Lock()
	delete(s.cache, uid)
	s.mu.Unlock()
	if err := os.Remove(s.path(uid)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) path(uid string) string {
	// 文件名不含 realm，同 UID 在两个区域被视为同一账号；
	// 真实归属由 Credentials.Realm 字段区分。
	return filepath.Join(s.dir, "wb-"+uid+".json")
}
