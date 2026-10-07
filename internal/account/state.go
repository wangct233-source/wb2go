package account

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/wb2go/wb2go/internal/config"
)

// State 是池状态的持久化格式。
//
// 刻意只存"跨重启需要延续"的状态：
//   - 积分：重启后仍需知道余额，避免立刻把触底号派出去
//   - 冷却与熔断：重启不该让刚被限流的号立刻复活
//   - 失败计数：熔断退避的 level 需要连续性
//
// 不存：InFlight（进程内概念）、DailyTokens（可从 usage JSONL 重算）。
type State struct {
	Version int               `json:"version"`
	Entries map[string]*Entry `json:"entries"`
	// 粘性绑定也落盘：重启后老会话继续绑原账号，避免多轮对话上下文重置。
	Binds map[string]string `json:"binds,omitempty"`
}

const stateVersion = 1

// LoadState 从磁盘恢复池状态。文件不存在是正常情况（首次启动）。
func LoadState(path string) (*State, error) {
	st := &State{Version: stateVersion, Entries: map[string]*Entry{}, Binds: map[string]string{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("读取状态文件失败: %w", err)
	}
	if err := json.Unmarshal(raw, st); err != nil {
		// 状态文件损坏不阻塞启动：宁可从零开始，也不要让网关起不来。
		// 这里丢的只是冷却计时，账号凭证是独立的文件，没有风险。
		return &State{Version: stateVersion, Entries: map[string]*Entry{}, Binds: map[string]string{}}, nil
	}
	if st.Entries == nil {
		st.Entries = map[string]*Entry{}
	}
	for _, e := range st.Entries {
		if e.ModelCools == nil {
			e.ModelCools = map[string]time.Time{}
		}
	}
	return st, nil
}

// Restore 把持久化状态灌进池。
func (p *Pool) Restore(st *State) {
	p.mu.Lock()
	for uid, e := range st.Entries {
		cur, ok := p.entries[uid]
		if !ok {
			// 账号已从凭证目录删除，丢弃其状态
			continue
		}
		e.InFlight = 0 // 在途不跨重启
		e.DailyTokens = 0
		cur.Credits, cur.CreditsKnown, cur.ExpireAt = e.Credits, e.CreditsKnown, e.ExpireAt
		cur.CoolUntil, cur.CoolKind = e.CoolUntil, e.CoolKind
		cur.BreakerUntil, cur.DegradeUntil = e.BreakerUntil, e.DegradeUntil
		cur.Fails, cur.SoftStreak = e.Fails, e.SoftStreak
		cur.ConsecFails, cur.SessionDeadFails = e.ConsecFails, e.SessionDeadFails
		cur.LastUsedAt, cur.LastCheckin = e.LastUsedAt, e.LastCheckin
		cur.LastError = e.LastError
		if e.Disabled {
			cur.Disabled, cur.DisabledReason = true, e.DisabledReason
		}
		if len(e.ModelCools) > 0 {
			cur.ModelCools = map[string]time.Time{}
			for k, v := range e.ModelCools {
				cur.ModelCools[k] = v
			}
		}
	}
	p.mu.Unlock()
}

// RestoreBinds 恢复粘性绑定。
func (s *StickyRouter) RestoreBinds(binds map[string]string, ttl time.Duration) {
	s.mu.Lock()
	exp := time.Now().Add(ttl)
	for k, uid := range binds {
		if k != "" && uid != "" {
			s.binds[k] = &binding{uid: uid, expiresAt: exp}
		}
	}
	s.mu.Unlock()
}

// Save 原子落盘池状态 + 粘性绑定。周期调用（如 30 秒一次）。
func (p *Pool) Save(path string, sticky *StickyRouter, stickyTTL time.Duration) error {
	p.mu.RLock()
	st := &State{Version: stateVersion, Entries: map[string]*Entry{}}
	for uid, e := range p.entries {
		cp := *e
		cp.InFlight, cp.DailyTokens = 0, 0
		st.Entries[uid] = &cp
	}
	p.mu.RUnlock()

	if sticky != nil {
		sticky.mu.RLock()
		now := time.Now()
		for k, b := range sticky.binds {
			if now.Before(b.expiresAt) {
				st.Binds[k] = b.uid
			}
		}
		sticky.mu.RUnlock()
		_ = stickyTTL
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, append(data, '\n'), 0o600)
}
