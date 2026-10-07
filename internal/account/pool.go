package account

import (
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wb2go/wb2go/internal/config"
)

// 账号处置动作。由调用方（proxy）根据动作决定是否轮转到下一个账号。
type Action int

const (
	// ActionSuccess：本次调用成功，清理该账号的错误状态。
	ActionSuccess Action = iota
	// ActionRetry：换一个账号重试（限流、会话失效等账号级问题）。
	ActionRetry
	// ActionFailFast：不换号，直接把错误返回客户端。
	//
	// 用于"换任何号都会撞同一堵墙"的错误：内容审核、请求体超限、图片非法。
	// 这类错误不是账号的问题，轮转纯属浪费额度还污染其他账号的计数。
	ActionFailFast
)

// 账号级冷却原因。分开的意义在于"解冻时机不同"：
// 余额耗尽要等签到回血，限流只要等退避到期。
type CoolKind int

const (
	CoolNone        CoolKind = iota
	CoolSoft                 // 软限流：指数退避
	CoolHard                 // 余额耗尽：硬冷却到次日 04:00
	CoolShort                // 上游 404 等瞬时问题：固定短冷却
	CoolBreaker              // 熔断
	CoolDegrade              // 连败降权
	CoolModel                // 模型级冷却（只挡某个模型）
	CoolDisabled             // 人工禁用
	CoolSessionDead          // 会话失效计数超阈值
)

func (k CoolKind) String() string {
	switch k {
	case CoolSoft:
		return "限流冷却"
	case CoolHard:
		return "积分冷却"
	case CoolShort:
		return "短冷却"
	case CoolBreaker:
		return "熔断"
	case CoolDegrade:
		return "降权"
	case CoolDisabled:
		return "已禁用"
	case CoolSessionDead:
		return "会话失效"
	}
	return "可用"
}

// Entry 是一个账号在池中的完整状态。
//
// 状态分成四组互不干扰的字段，这是整个池设计的核心：
//
//	人工状态：Disabled / DisabledReason（不随时间自愈）
//	时间状态：CoolUntil / BreakerUntil / DegradeUntil（三者并列，谁晚听谁的）
//	模型状态：ModelCools（某模型被限流，账号本身仍可用）
//	计数状态：Fails / SoftStreak / ConsecFails / SessionDeadFails
//
// 分家的理由：不同冷却原因的恢复条件完全不同，如果揉成一个 cooldownUntil，
// "余额恢复解冻"就不得不清掉所有冷却记录（包括还没到期的限流冷却），
// 而"限流冷却被余额刷新解冻"会让 5 分钟一次的刷新把退避寿命压到刷新周期内。
type Entry struct {
	UID          string  `json:"uid"`
	Nickname     string  `json:"nickname,omitempty"`
	Realm        string  `json:"realm"`
	Identity     string  `json:"identity,omitempty"`
	Credits      float64 `json:"credits"`
	CreditsKnown bool    `json:"credits_known"`
	ExpireAt     int64   `json:"expire_at,omitempty"` // 最近一批积分的到期时间

	Disabled       bool   `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`

	CoolUntil    time.Time `json:"cool_until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind,omitempty"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
	DegradeUntil time.Time `json:"degrade_until,omitempty"`

	// ModelCools 是 模型 → 冷却截止。
	// 上游 code 6004 表示"该模型用量超限"，不是账号整体被限流；
	// 若按账号级冷却处理，会让"换个模型就能用"的号被整体摘出池子。
	ModelCools map[string]time.Time `json:"model_cools,omitempty"`

	Fails            int `json:"fails,omitempty"`
	SoftStreak       int `json:"soft_streak,omitempty"`
	ConsecFails      int `json:"consec_fails,omitempty"`
	SessionDeadFails int `json:"session_dead_fails,omitempty"`

	InFlight    int    `json:"in_flight,omitempty"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
	LastCheckin int64  `json:"last_checkin,omitempty"`
	LastError   string `json:"last_error,omitempty"`

	// 每日 token 计数由 usage 包回填，不在持久化里（可从 JSONL 重算）。
	DailyTokens int64 `json:"-"`

	// 进而在途租约时的持有者标记，防止 Release 误清别人的租约。
	lease int64
}

// SessionDeadThreshold：会话失效要连续几次才永久禁用。
//
// 为什么不是 1 次：上游返回 session not found 时，绝大多数情况是网络抖动、
// 连接闪断或 token 刷新竞态导致的一次性失败。禁用一次的代价是用户必须
// 重新走一遍 OAuth 登录，而误判一次几乎必然发生。
const SessionDeadThreshold = 3

// TodayCreditsFloor 是积分保底：余额低于该值时，实测收费模型不再参与选号。
//
// 目的不是省钱，而是"留余额给免费模型用"。最坏情况是收费模型把余额打穿，
// 连带免费模型也 402 冷却到次日签到，等于全池停摆近半天。
const TodayCreditsFloor = 0

// Available 返回账号此刻是否可被选中（不含 realm / 粘性 / 在途判断）。
func (e *Entry) Available(now time.Time) bool {
	if e.Disabled {
		return false
	}
	if now.Before(e.BreakerUntil) || now.Before(e.DegradeUntil) || now.Before(e.CoolUntil) {
		return false
	}
	return true
}

// AvailableFor 判断账号对某个模型是否可用。
func (e *Entry) AvailableFor(now time.Time, model string) bool {
	if !e.Available(now) {
		return false
	}
	if until, ok := e.ModelCools[model]; ok {
		if now.Before(until) {
			return false
		}
		delete(e.ModelCools, model) // 惰性清理：到期即删，不额外跑 GC
	}
	return true
}

// InFlightFull 判断在途租约是否已占满。limit 为 0 表示不限。
func (e *Entry) InFlightFull(limit int) bool { return limit > 0 && e.InFlight >= limit }

// BlockedByQuota 返回账号被额度策略挡住的说明，空串表示可接单。
func (e *Entry) BlockedByQuota(cfg *config.Config, now time.Time) string {
	if cfg.ReserveCredits > 0 && e.CreditsKnown && e.Credits <= float64(cfg.ReserveCredits) {
		return "保留积分"
	}
	if cfg.DailyTokenLimit > 0 && e.DailyTokens >= int64(cfg.DailyTokenLimit) {
		return "日限额"
	}
	return ""
}

// StatusLabel 返回面板上展示的状态文案。
func (e *Entry) StatusLabel(now time.Time) string {
	switch {
	case e.Disabled:
		return "已禁用"
	case now.Before(e.BreakerUntil):
		return "熔断"
	case now.Before(e.DegradeUntil):
		return "降权"
	case now.Before(e.CoolUntil):
		return e.CoolKind.String()
	}
	return "可用"
}

// CoolRemaining 返回冷却剩余秒数，未冷却时为 0。
func (e *Entry) CoolRemaining(now time.Time) int64 {
	var latest time.Time
	for _, t := range []time.Time{e.CoolUntil, e.BreakerUntil, e.DegradeUntil} {
		if t.After(latest) {
			latest = t
		}
	}
	if d := latest.Sub(now); d > 0 {
		return int64(d.Seconds())
	}
	return 0
}

// Pool 是账号池。所有对 Entry 的修改都在池锁保护下进行。
type Pool struct {
	mu      sync.RWMutex
	entries map[string]*Entry
	cursor  int // round-robin 游标
	cfg     *config.Config

	// waf 闸门：WAF 403 拦的是网关出口 IP 而不是账号。
	// 60 秒窗口内不同 UID 命中 2 次即判 IP 级拦截并整池停摆 60 秒。
	// 单个账号反复 403 永不触发——那是账号问题，不是 IP 问题。
	wafIPs   map[string]*wafWindow
	inFlight int

	rng *rand.Rand
}

type wafWindow struct {
	hits   map[string]time.Time // uid → 最近命中时间
	active time.Time            // 拦截截止；零值表示未激活
}

// NewPool 建一个空池。
func NewPool(cfg *config.Config) *Pool {
	return &Pool{
		entries: map[string]*Entry{},
		cfg:     cfg,
		wafIPs:  map[string]*wafWindow{},
		// 固定种子：让同一份负载下的选号可复现，便于排查"为什么总是这个号"。
		// 生产环境不需要密码学强度，也不需要真随机。
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SetConfig 换配置快照（面板热更新时调用）。
func (p *Pool) SetConfig(cfg *config.Config) {
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

// Upsert 插入或更新一个账号。凭证已存在时保留其运行时状态。
func (p *Pool) Upsert(uid, nickname, realm, identity string) *Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		e.Nickname, e.Realm, e.Identity = nickname, realm, identity
		return e
	}
	e := &Entry{
		UID: uid, Nickname: nickname, Realm: realm, Identity: identity,
		ModelCools: map[string]time.Time{},
	}
	p.entries[uid] = e
	return e
}

// Get 取账号（返回的是池内部指针，仅供只读观测使用）。
func (p *Pool) Get(uid string) (*Entry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entries[uid]
	return e, ok
}

// Remove 摘除账号。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	delete(p.entries, uid)
	p.mu.Unlock()
}

// List 返回按 UID 排序的账号快照（面板表格用）。
func (p *Pool) List() []*Entry {
	p.mu.RLock()
	out := make([]*Entry, 0, len(p.entries))
	for _, e := range p.entries {
		// 复制一份，避免把内部指针交给面板后被并发改写。
		cp := *e
		cp.ModelCools = map[string]time.Time{}
		for k, v := range e.ModelCools {
			cp.ModelCools[k] = v
		}
		out = append(out, &cp)
	}
	p.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// SetCredits 回写账号余额与最近到期时间。
func (p *Pool) SetCredits(uid string, remain float64, expireAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		e.Credits, e.CreditsKnown, e.ExpireAt = remain, true, expireAt
	}
}

// Healthy 统计可用账号数。
func (p *Pool) Healthy() (healthy, total int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.entries {
		total++
		if e.Available(now) {
			healthy++
		}
	}
	return
}

// Pick 选一个能处理该模型的账号。
//
// 选号管线（顺序不可换）：
//  1. 过滤：WAF 拦截时整池不可用；过滤 try 过的、realm 不匹配的、额度不足的、
//     在途占满的、模型级限流的
//  2. 最早到期优先：窗口内还有积分的账号按"最近到期时间升序"排前面，
//     避免一批快过期的积分白白作废
//  3. 加权随机：weight = 1 + 积分占比×10 + 闲置补偿
//     Top-5 短名单 + 名单内抽签，而不是直接取第一名——
//     纯排序会让积分最多的号承担几乎全部流量
func (p *Pool) Pick(realm, model string, try map[string]bool) *Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cfg := p.cfg

	if p.wafBlocked(now) {
		return nil
	}

	limit := cfg.MaxInFlight
	if realm == "intl" && cfg.GlobalInFlight > 0 {
		// 国际版 WAF 更紧，压低单号在途上限
		limit = cfg.GlobalInFlight
	}

	var cands []*Entry
	for uid, e := range p.entries {
		if try[uid] || e.Realm != realm {
			continue
		}
		if !e.AvailableFor(now, model) || e.InFlightFull(limit) {
			continue
		}
		if e.BlockedByQuota(cfg, now) != "" {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全被挡住时的兜底：取最早解冻的那个顶班。
		// 好过直接返回 503 —— 让请求去等一会，总好过立刻失败。
		if e := p.pickEarliestUnblockLocked(now, realm, model, try, limit); e != nil {
			return e
		}
		return nil
	}

	sort.SliceStable(cands, func(i, j int) bool { return cands[i].UID < cands[j].UID })

	if cfg.PreferExpiring && cfg.ExpiringSoon > 0 {
		cands = p.sortExpiringFirst(cands, now, cfg.ExpiringSoon)
	}

	maxCredits := 0.0
	for _, e := range cands {
		if e.Credits > maxCredits {
			maxCredits = e.Credits
		}
	}

	shortlist := cands
	if len(cands) > 5 {
		shortlist = cands[:5]
	}

	// 等权重洗牌：若直接按 UID 排序取前 5，并列时排后面的号永远进不了短名单。
	// 实测这类惊群会让某个号稳定承担 79/100 的流量。
	p.rng.Shuffle(len(shortlist), func(i, j int) {
		shortlist[i], shortlist[j] = shortlist[j], shortlist[i]
	})

	totalW := 0.0
	weights := make([]float64, len(shortlist))
	for i, e := range shortlist {
		w := p.weightOf(e, maxCredits, now)
		weights[i] = w
		totalW += w
	}
	if totalW <= 0 {
		return shortlist[0]
	}
	// 防惊群：跳过 100ms 内刚被选中的账号，避免同一瞬间所有请求压到同一个号。
	r := p.rng.Float64() * totalW
	var skipped *Entry
	for i, e := range shortlist {
		skip := now.Sub(time.Unix(e.LastUsedAt, 0)) < 100*time.Millisecond
		if skip && skipped == nil {
			skipped = e
			continue
		}
		r -= weights[i]
		if r <= 0 {
			return e
		}
	}
	if skipped != nil {
		return skipped
	}
	return shortlist[len(shortlist)-1]
}

// weightOf 计算选号权重。数值越大越优先。
//
// 三个分量：
//   - 基础 1：保证没积分的号也有非零机会，不至于完全饿死
//   - 积分占比 ×10：积分多的号能扛更多流量
//   - 闲置补偿：闲置越久补偿越高，兼顾负载摊开
func (p *Pool) weightOf(e *Entry, maxCredits float64, now time.Time) float64 {
	cfg := p.cfg
	w := 1.0
	if maxCredits > 0 && e.CreditsKnown && e.Credits > 0 {
		w += (e.Credits / maxCredits) * 10
	}
	if e.LastUsedAt > 0 {
		idleH := now.Sub(time.Unix(e.LastUsedAt, 0)).Hours()
		comp := idleH * cfg.IdleWeightPerH
		if cap := cfg.IdleWeightMax; cap > 0 && comp > cap {
			comp = cap
		}
		w += comp
	} else {
		// 从未使用过：给满分补偿，否则新号会一直被排到最后
		if cap := cfg.IdleWeightMax; cap > 0 {
			w += cap
		}
	}
	return w
}

// sortExpiringFirst 把快过期积分的账号排到前面。
// 只在 expiringSoon 小时窗口内参与，避免为了"抢"远期积分而放弃即将作废的。
func (p *Pool) sortExpiringFirst(cands []*Entry, now time.Time, windowH int) []*Entry {
	cutoff := now.Add(time.Duration(windowH) * time.Hour)
	var fresh, stale []*Entry
	for _, e := range cands {
		if e.ExpireAt > now.Unix() && e.ExpireAt <= cutoff.Unix() {
			fresh = append(fresh, e)
		} else {
			stale = append(stale, e)
		}
	}
	if len(fresh) == 0 {
		return cands
	}
	// 同到期时间时，剩余积分多的优先（先花快到期的那一批）
	sort.SliceStable(fresh, func(i, j int) bool {
		if fresh[i].ExpireAt != fresh[j].ExpireAt {
			return fresh[i].ExpireAt < fresh[j].ExpireAt
		}
		return fresh[i].Credits > fresh[j].Credits
	})
	return append(fresh, stale...)
}

// pickEarliestUnblockLocked 在全冷却时挑最早解冻的账号顶班。
//
// 兜底的价值：所有号都在冷却时，让请求去等一会好过立刻返回 503。
// 但必须守住三条底线，否则"顶班"会变成"绕过治理"：
//   - 在途已满的号不顶班（否则等于给超载的号再加压）
//   - 被额度策略挡住的号不顶班（绕过后它一接单就立刻 402）
//   - 余额耗尽的号不顶班（顶班等于把 402 换个地方发生）
func (p *Pool) pickEarliestUnblockLocked(now time.Time, realm, model string, try map[string]bool, limit int) *Entry {
	var best *Entry
	var bestAt time.Time
	for uid, e := range p.entries {
		if try[uid] || e.Realm != realm || e.Disabled {
			continue
		}
		if e.InFlightFull(limit) {
			continue
		}
		if e.BlockedByQuota(p.cfg, now) != "" {
			continue
		}
		// 余额耗尽的号不顶班
		if e.CoolKind == CoolHard {
			continue
		}
		// 模型级冷却的号也不能顶班：这正是当前请求要用的模型
		if until, ok := e.ModelCools[model]; ok && now.Before(until) {
			continue
		}
		unblock := e.CoolUntil
		if e.BreakerUntil.After(unblock) {
			unblock = e.BreakerUntil
		}
		if e.DegradeUntil.After(unblock) {
			unblock = e.DegradeUntil
		}
		if best == nil || unblock.Before(bestAt) {
			best, bestAt = e, unblock
		}
	}
	return best
}

// Acquire 拿在途租约。用 CAS 保证并发下不会超发。
func (p *Pool) Acquire(uid string, realm string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return false
	}
	limit := p.cfg.MaxInFlight
	if realm == "intl" && p.cfg.GlobalInFlight > 0 {
		limit = p.cfg.GlobalInFlight
	}
	if limit > 0 && e.InFlight >= limit {
		return false
	}
	e.InFlight++
	e.lease++
	p.inFlight++
	return true
}

// Release 归还租约。
func (p *Pool) Release(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok && e.InFlight > 0 {
		e.InFlight--
		p.inFlight--
	}
}

// InFlightCount 当前全局在途数（面板展示）。
func (p *Pool) InFlightCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.inFlight
}

// Report 按动作更新账号状态。这是所有状态迁移的唯一入口。
//
// 调用方传 now 而不是让函数内部取时间，方便测试构造确定的时间线。
func (p *Pool) Report(uid, model string, action Action, code int, errMsg string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return
	}
	e.LastError = truncate(errMsg, 200)

	if action == ActionSuccess {
		e.Fails, e.SoftStreak, e.ConsecFails, e.SessionDeadFails = 0, 0, 0, 0
		e.LastUsedAt = now.Unix()
		// 余额冷却之外的冷却（限流/短冷却）在成功时立刻解除：
		// 上游能正常回包，说明这个号其实还活着。
		if e.CoolKind != CoolHard {
			e.CoolUntil, e.CoolKind = time.Time{}, CoolNone
		}
		if e.Disabled && e.CoolKind == CoolSessionDead {
			// 刷新成功说明会话其实有效（之前是抖动），撤销禁用
			e.Disabled, e.DisabledReason, e.CoolKind = false, "", CoolNone
		}
		return
	}

	// 内容审核类错误由调用方传 ActionFailFast 处理，这里不会走到。
	switch action {
	case ActionFailFast:
		return
	case ActionRetry:
		e.ConsecFails++
		// 会话失效：连续 3 次才禁用，单次多为抖动
		if isSessionDead(code, errMsg) {
			e.SessionDeadFails++
			if e.SessionDeadFails >= SessionDeadThreshold && !e.Disabled {
				e.Disabled = true
				e.DisabledReason = "会话连续失效 " + itoa(e.SessionDeadFails) + " 次，请重新登录"
				e.CoolKind = CoolSessionDead
			}
			return
		}
		// 限流：软冷却按连续次数指数退避（与熔断器并存的第二条升级线）
		if code == 429 {
			e.SoftStreak++
			d := backoff(p.cfg.SoftRateD, e.SoftStreak-1, p.cfg.SoftRateMaxD)
			e.CoolUntil, e.CoolKind = now.Add(d), CoolSoft
			return
		}
		// 余额耗尽：硬冷却到次日 04:00，等签到回血
		if code == 402 || containsAny(errMsg, "余额不足", "积分不足", "额度已用尽", "insufficient") {
			e.CoolUntil, e.CoolKind = tomorrow4AM(now), CoolHard
			e.Fails = 0
			return
		}
		// 上游 404：固定 60s 短冷却，不参与指数退避（重试意义不大）
		if code == 404 {
			e.CoolUntil, e.CoolKind = now.Add(60*time.Second), CoolShort
			return
		}
		// 5xx 与传输层错误：喂连续失败计数，达阈值熔断
		e.Fails++
		if p.cfg.BreakerThreshold > 0 && e.Fails >= p.cfg.BreakerThreshold {
			lvl := e.Fails - p.cfg.BreakerThreshold
			e.BreakerUntil = now.Add(backoff(p.cfg.BreakerBaseD, lvl, p.cfg.BreakerMaxD))
		}
		if p.cfg.DegradeThreshold > 0 && e.ConsecFails >= p.cfg.DegradeThreshold {
			lvl := e.ConsecFails - p.cfg.DegradeThreshold
			e.DegradeUntil = now.Add(backoff(p.cfg.DegradeBaseD, lvl, p.cfg.DegradeMaxD))
		}
	}
}

// NoteModelCool 只冷却某个模型，账号本身保持可用。
func (p *Pool) NoteModelCool(uid, model string, until time.Time) {
	if model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		if e.ModelCools == nil {
			e.ModelCools = map[string]time.Time{}
		}
		e.ModelCools[model] = until
	}
}

// ClearModelCool 清掉某个模型的冷却。
func (p *Pool) ClearModelCool(uid, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		delete(e.ModelCools, model)
	}
}

// Revive 人工复活一个账号（清除禁用与全部冷却）。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return false
	}
	e.Disabled, e.DisabledReason = false, ""
	e.CoolUntil, e.CoolKind = time.Time{}, CoolNone
	e.BreakerUntil, e.DegradeUntil = time.Time{}, time.Time{}
	e.Fails, e.SoftStreak, e.ConsecFails, e.SessionDeadFails = 0, 0, 0, 0
	return true
}

// ReviveIfSessionOnly 在 token 刷新成功后调用。
//
// 只撤销"会话失效"导致的禁用，不动其他禁用原因 ——
// 用户手动停用的账号不该被后台任务悄悄复活。
func (p *Pool) ReviveIfSessionOnly(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok || !e.Disabled {
		return
	}
	if e.CoolKind != CoolSessionDead {
		return
	}
	e.Disabled, e.DisabledReason, e.CoolKind = false, "", CoolNone
	e.SessionDeadFails = 0
}

// Disable 人工禁用。
func (p *Pool) Disable(uid, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		e.Disabled, e.DisabledReason, e.CoolKind = true, reason, CoolDisabled
		return true
	}
	return false
}

// SetEnabled 切换启用态。
func (p *Pool) SetEnabled(uid string, enabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return false
	}
	if enabled {
		e.Disabled, e.DisabledReason, e.CoolKind = false, "", CoolNone
	} else {
		e.Disabled, e.DisabledReason, e.CoolKind = true, "已手动停用", CoolDisabled
	}
	return true
}

// SetIdentity 切换账号的出站身份。
func (p *Pool) SetIdentity(uid, identity string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[uid]
	if !ok {
		return false
	}
	e.Identity = identity
	return true
}

// TouchDailyTokens 累加当日 token 计数（由 usage 层在记账时调用）。
func (p *Pool) TouchDailyTokens(uid string, tokens int64) {
	if tokens <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[uid]; ok {
		e.DailyTokens += tokens
	}
}

// ResetDailyTokens 跨日时清零当日 token 计数。
func (p *Pool) ResetDailyTokens() {
	p.mu.Lock()
	for _, e := range p.entries {
		e.DailyTokens = 0
	}
	p.mu.Unlock()
}

// CreditFloorBlocked 判断账号是否被积分保底挡在收费模型之外。
//
// 目的：保住余额给免费模型用。全池都被挡时返回 false ——
// 此时若继续拦着，就是"谁都发不出请求"（网关回 503），不如放行让用户至少能用。
func (p *Pool) CreditFloorBlocked(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entries[uid]
	if !ok || p.cfg.CreditFloor <= 0 || !e.CreditsKnown {
		return false
	}
	return e.Credits < float64(p.cfg.CreditFloor)
}

// NoteWAF403 记录一次 WAF 403 并判断是否升级为 IP 级拦截。
func (p *Pool) NoteWAF403(uid string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 同一个出口 IP 的窗口由调用方按出口区分；此处用全局窗口。
	w := p.wafIPs[""]
	if w == nil {
		w = &wafWindow{hits: map[string]time.Time{}}
		p.wafIPs[""] = w
	}
	const win = 60 * time.Second
	// 清掉窗口外的记录
	for k, t := range w.hits {
		if now.Sub(t) > win {
			delete(w.hits, k)
		}
	}
	// 已在拦截中：不续期。保守做法，避免反复探测延长拦截。
	if w.active.After(now) {
		return true
	}
	w.hits[uid] = now
	if len(w.hits) >= 2 {
		w.active = now.Add(win)
		w.hits = map[string]time.Time{}
		return true
	}
	return false
}

func (p *Pool) wafBlocked(now time.Time) bool {
	w := p.wafIPs[""]
	return w != nil && w.active.After(now)
}

// WAFBlockedFor reports whether an IP level WAF block is active.
func (p *Pool) WAFBlockedFor(now time.Time) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.wafBlocked(now)
}

// SetIdentityFor 更新账号出站身份（供上游层调用）。
func (p *Pool) SetIdentityFor(uid, identity string) { p.SetIdentity(uid, identity) }

// backoff 计算指数退避时长，封顶到 max。
func backoff(base time.Duration, level int, maxDur time.Duration) time.Duration {
	if level < 0 {
		level = 0
	}
	d := base
	for i := 0; i < level; i++ {
		d *= 2
		if d >= maxDur || d <= 0 {
			return maxDur
		}
	}
	if d > maxDur {
		return maxDur
	}
	return d
}

// tomorrow4AM 计算次日 04:00 的时刻。
//
// 为什么是 04:00 而不是 00:00：给上游的日额度重置留 4 小时余量。
func tomorrow4AM(now time.Time) time.Time {
	n := now.Add(24 * time.Hour)
	return time.Date(n.Year(), n.Month(), n.Day(), 4, 0, 0, 0, now.Location())
}

func isSessionDead(code int, msg string) bool {
	if code != 401 && code != 403 {
		return false
	}
	return containsAny(msg, "session not found", "会话不存在", "12153", "offline user session")
}

func containsAny(s string, subs ...string) bool {
	if s == "" {
		return false
	}
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// jsonNumber 是 json.Number 的别名占位，保持 toStr 的类型开关可编译。
type jsonNumber = json.Number
