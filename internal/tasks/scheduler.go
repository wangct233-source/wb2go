package tasks

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Logger 是调度器输出日志的接口（由 main 包注入）。
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Scheduler 是整点调度器。
//
// 为什么分段等待而不是"睡到下一个整点"：
// 长睡会被系统休眠打断。Windows 的 Modern Standby、Linux 的 suspend、
// 容器所在主机的 hibernation 都会冻结定时器，导致醒来后要么丢一次执行，
// 要么在同一点上补跑一堆。一分钟一跳 + 墙钟重判的实现是幂等的，
// 错过就等下一次，补跑则由各任务的幂等逻辑消化。
type Scheduler struct {
	log     Logger
	handler Handler

	mu      sync.Mutex
	lastRun map[string]int64 // 任务名 → 最近一次触发的小时戳
	running map[string]bool
	stop    chan struct{}
	once    sync.Once

	// 手动触发用
	manualMu sync.Mutex
	manual   map[string]bool
}

// Handler 是各项任务的具体实现，由 main 包注入。
type Handler interface {
	// Accounts 返回当前可操作账号列表。
	Accounts() []Account
	// Checkin 对单个账号执行签到，返回是否实际领取。
	Checkin(ctx context.Context, a Account) (bool, string)
	// Credits 刷新单号余额。
	Credits(ctx context.Context, a Account) (float64, bool)
	// Keepalive 刷新单号 token，返回可读结果。
	Keepalive(ctx context.Context, a Account) (string, error)
	// Active 上报对话活跃（点亮连登）。
	Active(ctx context.Context, a Account) (string, error)
	// Travel 推进猫猫旅行状态机。
	Travel(ctx context.Context, a Account) string
	// Tasks 执行成长任务一键完成。
	Tasks(ctx context.Context, a Account) string
	// NightCat 夜猫子补足对话。
	NightCat(ctx context.Context, a Account) string
	// DailyChat 国际版每日活跃打卡。
	DailyChat(ctx context.Context, a Account) (string, error)
}

// Account 是任务引擎视角的账号（只读快照）。
type Account struct {
	UID      string
	Label    string
	Realm    string
	Credits  float64
	Disabled bool
}

// NewScheduler 建调度器。
func NewScheduler(log Logger, h Handler) *Scheduler {
	return &Scheduler{
		log: log, handler: h,
		lastRun: map[string]int64{},
		running: map[string]bool{},
		manual:  map[string]bool{},
		stop:    make(chan struct{}),
	}
}

// Start 启动调度协程。
func (s *Scheduler) Start() {
	go s.loop()
}

// Stop 停止调度。
func (s *Scheduler) Stop() { s.once.Do(func() { close(s.stop) }) }

// loop 主循环：每分钟检查一次是否到了某个任务的触发点。
func (s *Scheduler) loop() {
	s.log.Infof("调度器已启动（每分钟检查一次触发点）")
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.Tick(time.Now())
		}
	}
}

// Tick 执行一次调度检查（导出供测试与"立即执行"用）。
func (s *Scheduler) Tick(now time.Time) {
	// 手动触发的任务立即跑
	s.manualMu.Lock()
	m := map[string]bool{}
	for k, v := range s.manual {
		m[k] = v
	}
	s.manual = map[string]bool{}
	s.manualMu.Unlock()

	for name := range m {
		s.run(name, now, true)
	}

	hour := now.Hour()
	stamp := int64(now.Year()*10000+int(now.Month())*100+now.Day())*100 + int64(hour)
	if !s.shouldRun("checkin", stamp) {
		return
	}
	switch hour {
	case 9, 21:
		s.run("checkin", now, false)
		s.run("travel", now, false)
	case 10, 16:
		s.run("tasks", now, false)
		s.run("active", now, false)
	case 22:
		s.run("keepalive", now, false)
	case 1:
		s.run("nightcat", now, false)
	}
}

// shouldRun 判断该任务在这个小时是否该跑（同一小时只跑一次）。
func (s *Scheduler) shouldRun(name string, stamp int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRun[name] == stamp {
		return false
	}
	s.lastRun[name] = stamp
	return true
}

// Trigger 手动触发某个任务（面板"立即执行"按钮）。
func (s *Scheduler) Trigger(name string) {
	s.manualMu.Lock()
	s.manual[name] = true
	s.manualMu.Unlock()
	// 唤醒主循环，不必等满一分钟
	go s.manualKick()
}

func (s *Scheduler) manualKick() {
	time.Sleep(50 * time.Millisecond)
	s.Tick(time.Now())
}

// run 执行一类任务。同一类任务不会并发执行。
func (s *Scheduler) run(name string, now time.Time, manual bool) {
	s.mu.Lock()
	if s.running[name] {
		s.mu.Unlock()
		s.log.Warnf("任务 %s 上一轮尚未结束，跳过本次触发", name)
		return
	}
	s.running[name] = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running[name] = false
		s.mu.Unlock()
	}()

	kind := "定时"
	if manual {
		kind = "手动"
	}
	s.log.Infof("── 任务 %s（%s）开始", name, kind)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	accounts := s.handler.Accounts()
	var done, failed, skipped int
	for _, a := range accounts {
		if a.Disabled {
			skipped++
			continue
		}
		select {
		case <-s.stop:
			return
		default:
		}
		ok, msg := s.dispatch(ctx, name, a)
		switch {
		case ok:
			done++
			if msg != "" {
				s.log.Infof("  [%s] %s", a.Label, msg)
			}
		case msg == "跳过":
			skipped++
		default:
			failed++
			s.log.Warnf("  [%s] %s: %s", a.Label, name, msg)
		}
		// 账号间放慢节奏：上游按请求密度判罚，批量操作齐步走会触发风控。
		s.sleep(ctx, 800*time.Millisecond)
	}
	s.log.Infof("── 任务 %s 结束：成功 %d / 失败 %d / 跳过 %d", name, done, failed, skipped)
}

// dispatch 把一类任务分派到对应实现。
func (s *Scheduler) dispatch(ctx context.Context, name string, a Account) (bool, string) {
	switch name {
	case "checkin":
		if a.Realm != "cn" {
			return false, "跳过" // 签到仅国内版
		}
		ok, msg := s.handler.Checkin(ctx, a)
		if ok {
			return true, msg
		}
		return false, msg
	case "credits":
		bal, ok := s.handler.Credits(ctx, a)
		if !ok {
			return false, "刷新余额失败"
		}
		return true, fmt.Sprintf("余额 %.0f", bal)
	case "keepalive":
		msg, err := s.handler.Keepalive(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return true, msg
	case "active":
		if a.Realm != "cn" {
			return false, "跳过"
		}
		msg, err := s.handler.Active(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return true, msg
	case "travel":
		if a.Realm != "cn" {
			return false, "跳过"
		}
		return true, s.handler.Travel(ctx, a)
	case "tasks":
		if a.Realm != "cn" {
			return false, "跳过"
		}
		return true, s.handler.Tasks(ctx, a)
	case "nightcat":
		if a.Realm != "cn" {
			return false, "跳过"
		}
		return true, s.handler.NightCat(ctx, a)
	case "daily_chat":
		if a.Realm != "intl" {
			return false, "跳过" // 国际活跃打卡仅国际版
		}
		msg, err := s.handler.DailyChat(ctx, a)
		if err != nil {
			return false, err.Error()
		}
		return true, msg
	}
	return false, "未知任务: " + name
}

func (s *Scheduler) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	case <-s.stop:
	}
}

// Status 返回调度器状态（面板展示）。
type Status struct {
	Running   map[string]bool  `json:"running"`
	LastRun   map[string]int64 `json:"last_run"`
	Active    bool             `json:"active"`
	Timezone  string           `json:"timezone"`
	ServerNow string           `json:"server_time"`
}

// Status 取状态。
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	return Status{
		Running:   copyBoolMap(s.running),
		LastRun:   copyInt64Map(s.lastRun),
		Timezone:  now.Location().String(),
		ServerNow: now.Format("2006-01-02 15:04:05"),
	}
}

func copyBoolMap(src map[string]bool) map[string]bool {
	out := make(map[string]bool, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func copyInt64Map(src map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// JoinLog 把多行日志拼成一行（面板用）。
func JoinLog(parts []string, sep string) string {
	return strings.Join(parts, sep)
}
