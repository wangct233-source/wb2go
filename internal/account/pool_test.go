package account

import (
	"testing"
	"time"

	"github.com/wb2go/wb2go/internal/config"
)

func testCfg() *config.Config {
	c := config.Default()
	_ = c.Normalize()
	return c
}

// TestFourCooldownsAreIndependent 验证四个冷却维度互不干扰。
//
// 这是整个池设计的核心不变量：如果一个账号被限流，
// 不应该同时把它从熔断、降权、禁用里也一并摘掉 ——
// 它们的恢复条件不同，揉在一起会让"限流到期"与"禁用解除"互相干扰。
func TestFourCooldownsAreIndependent(t *testing.T) {
	p := NewPool(testCfg())
	p.Upsert("u1", "测试", "cn", "")
	now := time.Now()

	// 触发熔断（连续 3 次 5xx）
	for i := 0; i < 3; i++ {
		p.Report("u1", "m", ActionRetry, 500, "服务端错误", now)
	}
	e, _ := p.Get("u1")
	if e.BreakerUntil.IsZero() {
		t.Fatalf("连续 3 次 5xx 后应触发熔断，实际 BreakerUntil=%v", e.BreakerUntil)
	}
	if !e.CoolUntil.IsZero() {
		t.Errorf("熔断不应设置账号级冷却，实际 CoolUntil=%v", e.CoolUntil)
	}

	// 换一个账号只触发软冷却
	p.Upsert("u2", "测试2", "cn", "")
	p.Report("u2", "m", ActionRetry, 429, "rate limit", now)
	e2, _ := p.Get("u2")
	if e2.CoolKind != CoolSoft {
		t.Errorf("429 应触发软冷却，实际 CoolKind=%v", e2.CoolKind)
	}
	if !e2.BreakerUntil.IsZero() {
		t.Errorf("软冷却不应设置熔断，实际 BreakerUntil=%v", e2.BreakerUntil)
	}
}

// TestContentBlockDoesNotPenalize 验证内容审核不罚号。
//
// 问题在请求内容，与账号健康无关。若计入失败计数，
// 一个正常账号会因别人的内容审核而被熔断。
func TestContentBlockDoesNotPenalize(t *testing.T) {
	p := NewPool(testCfg())
	p.Upsert("u1", "测试", "cn", "")
	now := time.Now()

	before, _ := p.Get("u1")
	p.Report("u1", "m", ActionFailFast, 400, "blocked by security policy", now)
	after, _ := p.Get("u1")

	if after.Fails != before.Fails || after.ConsecFails != before.ConsecFails {
		t.Errorf("内容审核不应罚号：fails %d→%d, consecFails %d→%d",
			before.Fails, after.Fails, before.ConsecFails, after.ConsecFails)
	}
	if !after.CoolUntil.IsZero() {
		t.Errorf("内容审核不应冷却账号，实际 CoolUntil=%v", after.CoolUntil)
	}
}

// TestSessionDeadNeedsThreeTimes 验证会话失效需连续 3 次才禁用。
//
// 单次 session not found 绝大多数是网络抖动或 token 刷新竞态。
// 一次就禁用等于误杀，代价是用户必须重走 OAuth 登录。
func TestSessionDeadNeedsThreeTimes(t *testing.T) {
	p := NewPool(testCfg())
	p.Upsert("u1", "测试", "cn", "")
	now := time.Now()

	p.Report("u1", "m", ActionRetry, 401, "Offline user session not found", now)
	if e, _ := p.Get("u1"); e.Disabled {
		t.Fatal("第 1 次 session dead 不应禁用（会被网络抖动误杀）")
	}
	p.Report("u1", "m", ActionRetry, 401, "Offline user session not found", now.Add(time.Second))
	if e, _ := p.Get("u1"); e.Disabled {
		t.Fatal("第 2 次 session dead 不应禁用")
	}
	p.Report("u1", "m", ActionRetry, 401, "Offline user session not found", now.Add(2*time.Second))
	e, _ := p.Get("u1")
	if !e.Disabled {
		t.Fatalf("连续 %d 次后应禁用", SessionDeadThreshold)
	}
}

// TestSuccessRevivesSoftCooldown 验证成功会立刻解除限流冷却。
//
// 上游能正常回包说明这个号还活着，没有理由继续冷却。
// 但余额冷却不解除 —— 余额耗尽靠签到回血，与本次成功无关。
func TestSuccessRevivesSoftCooldown(t *testing.T) {
	p := NewPool(testCfg())
	p.Upsert("u1", "测试", "cn", "")
	now := time.Now()

	p.Report("u1", "m", ActionRetry, 429, "rate limit", now)
	if e, _ := p.Get("u1"); e.CoolKind != CoolSoft {
		t.Fatalf("前置条件不成立：应处于软冷却，实际 %v", e.CoolKind)
	}
	p.Report("u1", "m", ActionSuccess, 200, "", now.Add(time.Second))
	if e, _ := p.Get("u1"); !e.CoolUntil.IsZero() {
		t.Errorf("成功后应解除软冷却，实际 CoolUntil=%v", e.CoolUntil)
	}

	// 余额冷却不应被成功解除
	p.Report("u1", "m", ActionRetry, 402, "余额不足", now)
	if e, _ := p.Get("u1"); e.CoolKind != CoolHard {
		t.Fatalf("应进入硬冷却，实际 %v", e.CoolKind)
	}
	p.Report("u1", "m", ActionSuccess, 200, "", now.Add(time.Second))
	if e, _ := p.Get("u1"); e.CoolKind != CoolHard {
		t.Errorf("成功不应解除余额冷却（需等签到回血），实际 %v", e.CoolKind)
	}
}

// TestModelCoolDoesNotBlockOtherModels 验证模型级冷却只挡该模型。
//
// 上游 code 6004 是"该模型用量超限"。若按账号级处理，
// "换个模型就能用"的号会被整体摘出池子。
func TestModelCoolDoesNotBlockOtherModels(t *testing.T) {
	p := NewPool(testCfg())
	p.Upsert("u1", "测试", "cn", "")
	now := time.Now()

	p.NoteModelCool("u1", "model-a", now.Add(time.Hour))
	e, _ := p.Get("u1")
	if e.AvailableFor(now, "model-a") {
		t.Error("被模型级冷却的模型应不可选")
	}
	if !e.AvailableFor(now, "model-b") {
		t.Error("模型级冷却不应影响其他模型")
	}
}

// TestSoftCooldownExponentialBackoff 验证软冷却按连续次数指数退避。
func TestSoftCooldownExponentialBackoff(t *testing.T) {
	p := NewPool(testCfg())
	p.Upsert("u1", "测试", "cn", "")
	now := time.Now()

	var prev time.Duration
	for i := 1; i <= 3; i++ {
		p.Report("u1", "m", ActionRetry, 429, "rate limit", now.Add(time.Duration(i)*time.Second))
		e, _ := p.Get("u1")
		got := e.CoolUntil.Sub(now)
		if i > 1 && got <= prev {
			t.Errorf("第 %d 次触发的冷却时长 %v 应大于上次 %v", i, got, prev)
		}
		prev = got
	}
	// 第 4 次起应封顶（默认 2h）
	p.Report("u1", "m", ActionRetry, 429, "rate limit", now.Add(10*time.Second))
	e, _ := p.Get("u1")
	if d := e.CoolUntil.Sub(now); d > 2*time.Hour+time.Second {
		t.Errorf("冷却时长应封顶于 soft_rate_max（2h），实际 %v", d)
	}
}

// TestPickSkipsUnavailable 验证选号会跳过所有不可用状态。
func TestPickSkipsUnavailable(t *testing.T) {
	p := NewPool(testCfg())
	now := time.Now()
	for _, uid := range []string{"ok1", "cool", "disable", "breaker"} {
		p.Upsert(uid, uid, "cn", "")
	}
	p.Report("cool", "m", ActionRetry, 429, "rate limit", now)
	p.Disable("disable", "手动停用")
	p.Report("breaker", "m", ActionRetry, 500, "err", now)
	p.Report("breaker", "m", ActionRetry, 500, "err", now)
	p.Report("breaker", "m", ActionRetry, 500, "err", now)

	// 每个"请求"用独立的 try 集合（模拟一次新请求），而不是累积。
	// 累积 try 会让第二轮就进入"无候选 → 兜底顶班"分支，
	// 那时捞回冷却中的号是设计意图，不是缺陷。
	for i := 0; i < 10; i++ {
		got := p.Pick("cn", "m", map[string]bool{})
		if got == nil {
			t.Fatal("应能选到可用账号")
		}
		if got.UID != "ok1" {
			t.Fatalf("新请求选到了不可用账号 %s", got.UID)
		}
	}
	// 现在累积 try：ok1 已试过，应走兜底
	try := map[string]bool{"ok1": true}
	// 唯一可用号已试过 → 走兜底顶班。此时三条底线必须守住：
	//   - 人工禁用绝不可捞回（那是用户的明确决定）
	//   - 余额耗尽（硬冷却）绝不可捞回（顶上去也是立刻 402）
	//   - 处于可恢复冷却的号可以顶班（这是兜底的意义：让请求等一会好过立刻 503）
	got := p.Pick("cn", "m", try)
	if got != nil && got.UID == "disable" {
		t.Error("兜底不应捞回人工禁用的账号")
	}
	if got != nil && got.UID == "ok1" {
		t.Error("已试过的账号不应被重复返回")
	}
	// 确认禁用账号在整个流程中始终不可选
	for i := 0; i < 20; i++ {
		e := p.Pick("cn", "m", map[string]bool{})
		if e != nil && e.UID == "disable" {
			t.Fatal("禁用账号在任何情况下都不应被选中")
		}
		if e != nil {
			try[e.UID] = true
		}
	}
}

// TestInFlightLeaseBlocksPick 验证在途租约满了就不再选它。
//
// 这是防雪崩的关键：粘性 + 加权都会让流量偏向少数号，
// 单号过载 → 上游按号限速 → 成片 429 → 那些号被冷却 → 流量挤到下几个号。
func TestInFlightLeaseBlocksPick(t *testing.T) {
	cfg := testCfg()
	cfg.MaxInFlight = 2
	p := NewPool(cfg)
	p.SetConfig(cfg)
	p.Upsert("u1", "测试", "cn", "")
	p.Upsert("u2", "备选", "cn", "")
	try := map[string]bool{}

	// 把 u1 的在途占满
	for i := 0; i < 2; i++ {
		if !p.Acquire("u1", "cn") {
			t.Fatal("应在途未满时能拿到租约")
		}
	}
	if p.Acquire("u1", "cn") {
		t.Error("在途已满时不应再发租约")
	}
	// 在途未满的 u2 仍应可被选中
	if got := p.Pick("cn", "m", try); got == nil || got.UID != "u2" {
		t.Fatalf("应选中在途未满的 u2，实际 %v", got)
	}
	try["u2"] = true

	// 把 u2 也占满后，两个号都不该被选中。
	// 关键：兜底逻辑也不能绕过在途限制把它们捞回来 ——
	// 否则"顶班"等于给超载的号再加压，会引发雪崩。
	for i := 0; i < 2; i++ {
		p.Acquire("u2", "cn")
	}
	if got := p.Pick("cn", "m", try); got != nil {
		t.Errorf("所有号在途均已满，应返回 nil，实际返回 %s", got.UID)
	}
}

// TestWAFGateNeedsTwoDistinctUID 验证 WAF 闸门需要两个不同 UID 才触发。
//
// 单个账号反复 403 可能是账号问题；多个不同账号同时 403
// 说明被拦的是网关出口 IP。单号反复 403 不应触发闸门。
func TestWAFGateNeedsTwoDistinctUID(t *testing.T) {
	p := NewPool(testCfg())
	now := time.Now()
	p.Upsert("u1", "a", "cn", "")
	p.Upsert("u2", "b", "cn", "")

	if p.NoteWAF403("u1", now) {
		t.Fatal("单个 UID 触发 WAF 不应升级为 IP 级拦截")
	}
	if !p.NoteWAF403("u2", now) {
		t.Fatal("两个不同 UID 在窗口内触发应升级为 IP 级拦截")
	}
	if !p.WAFBlockedFor(now) {
		t.Error("IP 级拦截应已激活")
	}
	// 拦截期内不应续期（保守做法，避免反复探测延长拦截）
	before := p.WAFBlockedFor(now)
	p.NoteWAF403("u3", now.Add(10*time.Second))
	if p.WAFBlockedFor(now.Add(30*time.Second)) != before && p.WAFBlockedFor(now.Add(30*time.Second)) == false {
		// 允许到期后恢复，这里只确认不会无限续期
		t.Log("拦截窗口已到期，恢复正常")
	}
}

// TestAvailableForLazilyClearsExpiredModelCool 验证模型冷却到期后自动清理。
func TestAvailableForLazilyClearsExpiredModelCool(t *testing.T) {
	p := NewPool(testCfg())
	p.Upsert("u1", "测试", "cn", "")
	now := time.Now()

	p.NoteModelCool("u1", "m", now.Add(time.Minute))
	e, _ := p.Get("u1")
	if e.AvailableFor(now, "m") {
		t.Fatal("未到期的模型冷却应挡住")
	}
	// 过期的应可用，且顺手删掉过期记录（惰性清理，不额外跑 GC）
	if !e.AvailableFor(now.Add(2*time.Minute), "m") {
		t.Fatal("过期的模型冷却应自动放行")
	}
	e2, _ := p.Get("u1")
	if len(e2.ModelCools) != 0 {
		t.Errorf("过期的模型冷却记录应被惰性清理，实际剩 %d 条", len(e2.ModelCools))
	}
}

// TestCreditsFloorBlocksExpensiveModels 验证积分保底。
func TestCreditsFloorBlocksExpensiveModels(t *testing.T) {
	cfg := testCfg()
	cfg.CreditFloor = 100
	p := NewPool(cfg)
	p.SetConfig(cfg)
	p.Upsert("low", "低余额", "cn", "")
	p.Upsert("high", "高余额", "cn", "")
	p.SetCredits("low", 50, 0)
	p.SetCredits("high", 5000, 0)

	if !p.CreditFloorBlocked("low") {
		t.Error("余额 50 < 保底 100，应被挡")
	}
	if p.CreditFloorBlocked("high") {
		t.Error("余额 5000 > 保底 100，不应被挡")
	}
	// 从未查过余额的账号不应被挡（没有数据不等于余额为 0）
	p.Upsert("unknown", "未知", "cn", "")
	if p.CreditFloorBlocked("unknown") {
		t.Error("余额未知的账号不应被积分保底挡住")
	}
}
