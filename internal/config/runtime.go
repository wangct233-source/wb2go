package config

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Runtime 给 Config 加上并发保护与变更通知。
//
// 为什么要单独一层：Config 本身是纯数据，必须能被自由拷贝
// （请求路径需要一份只读快照）。把 sync.RWMutex 放进 Config 会导致
// `cp := *c` 触发 "copies lock value"，go vet 直接报错。
//
// 分层之后：
//   - Config  = 数据，可拷贝，可序列化
//   - Runtime = 保护 + 通知，全局只有一个实例
type Runtime struct {
	mu      sync.RWMutex
	cfg     *Config
	refresh chan struct{} // 配置变更时关闭，通知等待者
}

// NewRuntime 包装一份配置。
func NewRuntime(cfg *Config) *Runtime {
	return &Runtime{cfg: cfg, refresh: make(chan struct{}, 1)}
}

// Get 返回当前配置指针。仅用于启动期装配与只读场景。
// 运行期请用 Snapshot 拿快照。
func (r *Runtime) Get() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// Snapshot 返回一份只读快照，供请求路径无锁读取。
//
// 深拷贝切片字段，避免调用方 append 时改到原件的底层数组。
func (r *Runtime) Snapshot() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Clone()
}

// Mutate 就地修改配置并通知等待者。
func (r *Runtime) Mutate(fn func(*Config)) {
	r.mu.Lock()
	fn(r.cfg)
	r.cfg.UpdatedAt = time.Now()
	r.mu.Unlock()
	r.notify()
}

// Merge 从一份 JSON 深合并进当前配置并落盘（面板保存设置走这条路径）。
//
// 深合并而不是整体替换：只更新提交涉及的键，用户手写的未知键原样保留。
// 装配期字段（listen / 目录路径）不热改。
func (r *Runtime) Merge(raw []byte) error {
	if !json.Valid(raw) {
		return fmt.Errorf("配置不是合法的 JSON")
	}
	r.mu.Lock()
	err := r.cfg.MergeFrom(raw)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	r.notify()
	return nil
}

// ReloadFrom 重新读取配置文件（面板"重载配置"按钮）。
func (r *Runtime) ReloadFrom(path string) error {
	next, err := Load(path)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cfg = next
	r.mu.Unlock()
	r.notify()
	return nil
}

// Refreshed 返回一个在配置变更时被填充信号的 channel。
// 用于"等配置变化再继续"这类场景：select { case <-Refreshed(): 重新读配置 }
func (r *Runtime) Refreshed() <-chan struct{} { return r.refresh }

func (r *Runtime) notify() {
	select {
	case r.refresh <- struct{}{}:
	default: // 已有待处理信号，不重复发
	}
}

// Clone 返回一份可安全修改的深拷贝。
func (c *Config) Clone() *Config {
	cp := *c
	cp.Schedule.CheckinHours = append([]int(nil), c.Schedule.CheckinHours...)
	cp.Schedule.TravelHours = append([]int(nil), c.Schedule.TravelHours...)
	cp.Schedule.ActivityHours = append([]int(nil), c.Schedule.ActivityHours...)
	cp.Schedule.KeepaliveHours = append([]int(nil), c.Schedule.KeepaliveHours...)
	cp.Schedule.NightCatHours = append([]int(nil), c.Schedule.NightCatHours...)
	cp.Schedule.TaskHours = append([]int(nil), c.Schedule.TaskHours...)
	return &cp
}
