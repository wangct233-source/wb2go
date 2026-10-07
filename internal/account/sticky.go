package account

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StickyRouter 实现会话粘性：同一会话尽量绑定同一账号。
//
// 为什么需要粘性（两个独立原因，缺一不可）：
//  1. 上游 prompt cache 是**账号级**的。换号等于换一份服务端上下文缓存，
//     多轮对话每一轮都要重新计费，既慢又贵。
//  2. 拟人。真实用户的一次会话属于同一台设备、同一个账号，
//     逐轮跳号是极易识别的批量特征。
type StickyRouter struct {
	mu      sync.RWMutex
	binds   map[string]*binding
	ttl     time.Duration
	byPfx   bool // 是否允许"内容前缀派生"兜底
	enabled bool

	// 换号惩罚：刚被解绑的键短时间内不重新绑回去，
	// 避免"解绑→重绑同一账号→又失败"的抖动循环。
	penalty map[string]time.Time
}

type binding struct {
	uid       string
	expiresAt time.Time
}

// NewStickyRouter 建粘性路由。
func NewStickyRouter(enabled bool, ttl time.Duration, byPrefix bool) *StickyRouter {
	return &StickyRouter{
		binds:   map[string]*binding{},
		penalty: map[string]time.Time{},
		ttl:     ttl, byPfx: byPrefix, enabled: enabled,
	}
}

// SetConfig 热更新开关与 TTL。
func (s *StickyRouter) SetConfig(enabled bool, ttl time.Duration, byPrefix bool) {
	s.mu.Lock()
	s.enabled, s.ttl, s.byPfx = enabled, ttl, byPrefix
	s.mu.Unlock()
}

// ExtractKey 从请求里提取会话键。
//
// 提取顺序刻意把 conversation 放在 user 前面，且 snake_case 优先于 camelCase：
// 用户 ID 粒度太粗——一个用户的所有并行对话会被钉到同一账号上，
// 那不是粘性，那是串台。
func ExtractKey(meta map[string]any, body map[string]any) string {
	if meta != nil {
		for _, k := range []string{
			"conversation_id", "conversationId", "conversation.id",
			"session_id", "sessionId",
		} {
			if v := firstString(meta, k); v != "" {
				return v
			}
		}
	}
	if body != nil {
		for _, k := range []string{
			"conversation_id", "conversationId", "conversation",
			"session_id", "sessionId",
		} {
			if v := firstString(body, k); v != "" {
				return v
			}
		}
		// OpenAI 前缀缓存字段：部分客户端（pi-ai 系）把会话 ID 放这里
		if v := firstString(body, "prompt_cache_key"); v != "" {
			return v
		}
	}
	return ""
}

// firstString 从 map 里按 "a.b" 或 "a" 形式取值，转成非空字符串。
func firstString(m map[string]any, key string) string {
	// 展平 key：metadata 里嵌 conversation 对象的情况
	if idx := strings.Index(key, "."); idx > 0 {
		if sub, ok := m[key[:idx]].(map[string]any); ok {
			return toStr(sub[key[idx+1:]])
		}
		return ""
	}
	return toStr(m[key])
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return s
		}
	case float64:
		return trimFloat(t)
	case json.Number:
		return t.String()
	}
	return ""
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// DerivePrefixKey 在客户端没给任何会话 ID 时，用 system + 首条 user 消息
// 派生一个会话键。
//
// 为什么取"前两条"而不是首条：首条 user 消息在一次会话内是恒定的，
// 因此同会话恒得同键；新会话自然是新键。取全部消息做哈希则每轮都会变，
// 派生出来的键等于随机数。
//
// 前缀统一加 "d-"，用于和真实会话 ID 区分，便于观测与清理。
func (s *StickyRouter) DerivePrefixKey(messages []any) string {
	s.mu.RLock()
	allow := s.byPfx
	s.mu.RUnlock()
	if !allow {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, m := range messages {
		if n >= 2 {
			break
		}
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" && role != "user" {
			continue
		}
		b.WriteString(role)
		b.WriteByte(':')
		b.WriteString(flattenContent(mm["content"]))
		b.WriteByte('\n')
		n++
	}
	if b.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "d-" + hex.EncodeToString(sum[:8])
}

// flattenContent 把 content（字符串或内容块数组）压平成纯文本。
func flattenContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// Resolve 返回该会话键已绑定的账号，未绑定或已过期返回空串。
func (s *StickyRouter) Resolve(key string, now time.Time) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return ""
	}
	b, ok := s.binds[key]
	if !ok {
		return ""
	}
	if now.After(b.expiresAt) {
		delete(s.binds, key)
		return ""
	}
	// TTL 滚动续期：会话还在活跃就不断延长
	b.expiresAt = now.Add(s.ttl)
	return b.uid
}

// Bind 把会话键绑到指定账号。请求成功后调用，让绑定跟随"实际成功的那个号"。
func (s *StickyRouter) Bind(key, uid string, now time.Time) {
	if key == "" || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return
	}
	s.binds[key] = &binding{uid: uid, expiresAt: now.Add(s.ttl)}
	delete(s.penalty, key)
}

// Unbind 解绑（请求失败时调用，防止下一轮死撞同一个坏号）。
func (s *StickyRouter) Unbind(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.binds, key)
	// 记一个短惩罚窗口，避免立刻重新绑回同一个号
	s.penalty[key] = time.Now().Add(10 * time.Second)
}

// ShouldBind 判断当前是否允许建立新绑定（惩罚窗口内不允许）。
func (s *StickyRouter) ShouldBind(key string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.penalty[key]; ok && now.Before(t) {
		return false
	}
	return true
}

// GC 清理过期绑定。由后台协程按 gcInterval 周期调用。
func (s *StickyRouter) GC(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, b := range s.binds {
		if now.After(b.expiresAt) {
			delete(s.binds, k)
			n++
		}
	}
	for k, t := range s.penalty {
		if now.After(t) {
			delete(s.penalty, k)
		}
	}
	return n
}

// Count 当前绑定数（面板展示）。
func (s *StickyRouter) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.binds)
}

// Reset 清空全部绑定。
func (s *StickyRouter) Reset() {
	s.mu.Lock()
	s.binds = map[string]*binding{}
	s.penalty = map[string]time.Time{}
	s.mu.Unlock()
}
