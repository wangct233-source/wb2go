package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record 是一次请求的记账条目。
//
// 隐私约束：这里不存提示词、不存响应正文、不存任何 token 明文。
// 只留"能回答"这个问题的字段：谁、什么时候、用了哪个模型、多少 token、
// 哪个 Key、结果如何。
type Record struct {
	Timestamp     int64   `json:"ts"`
	UID           string  `json:"uid,omitempty"`
	KeyName       string  `json:"key,omitempty"`
	Model         string  `json:"model,omitempty"`
	Realm         string  `json:"realm,omitempty"`
	Identity      string  `json:"identity,omitempty"`
	Stream        bool    `json:"stream,omitempty"`
	Status        int     `json:"status"`
	Outcome       string  `json:"outcome"` // completed / error / aborted
	ErrorKind     string  `json:"errkind,omitempty"`
	PromptTok     int     `json:"pt,omitempty"`
	CompletionTok int     `json:"ct,omitempty"`
	CachedTok     int     `json:"cachet,omitempty"`
	ReasoningTok  int     `json:"rt,omitempty"`
	TotalTok      int     `json:"tot,omitempty"`
	Credits       float64 `json:"credits,omitempty"`
	TTFBms        int64   `json:"ttfb,omitempty"`
	Durationms    int64   `json:"dur,omitempty"`
	RequestID     string  `json:"rid,omitempty"`
	ClientIP      string  `json:"ip,omitempty"`
	UserAgent     string  `json:"ua,omitempty"`
}

// DayStat 是按天分桶的统计。
type DayStat struct {
	Date       string               `json:"date"`
	Requests   int                  `json:"req"`
	Completed  int                  `json:"ok"`
	Failed     int                  `json:"fail"`
	Prompt     int                  `json:"pt"`
	Completion int                  `json:"ct"`
	Cached     int                  `json:"cachet"`
	Reasoning  int                  `json:"rt"`
	Total      int                  `json:"tot"`
	Credits    float64              `json:"credits"`
	Models     map[string]ModelStat `json:"models,omitempty"`
}

// ModelStat 是单模型统计。
type ModelStat struct {
	Requests int     `json:"req"`
	Failed   int     `json:"fail"`
	Total    int     `json:"tot"`
	Credits  float64 `json:"credits"`
}

// Store 是用量存储。
//
// 双写：JSONL 追加（明细，可逐条检索）+ 内存按天分桶（聚合，供图表）。
// JSONL 不做无上限轮转是刻意的取舍——日志里的请求 ID 是排查问题的锚点，
// 删掉就查不到了。代价是文件会增长，用每天 roll 一次来控制。
type Store struct {
	mu    sync.RWMutex
	path  string
	days  map[string]*DayStat
	total DayStat
	// dailyTokens: uid → uid → 当日 token（给账号池做日限额判定）
	dailyTokens map[string]map[string]int64
	day         string
	maxRecords  int
	logFile     *os.File
	records     []Record // 环形缓冲，供给面板"最近请求"
	ringPos     int
}

// NewStore 建存储并从 JSONL 恢复聚合。
func NewStore(path string) (*Store, error) {
	s := &Store{
		path: path, days: map[string]*DayStat{},
		dailyTokens: map[string]map[string]int64{},
		day:         today(),
		maxRecords:  500,
		records:     make([]Record, 500),
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	return s, nil
}

func today() string { return time.Now().Format("2006-01-02") }

// replay 读历史 JSONL 重建聚合。
//
// 失败不阻塞启动：日志损坏最多让统计从头开始，不影响网关可用性。
func (s *Store) replay() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		s.apply(r)
	}
	return nil
}

// Record 记一笔。
func (s *Store) Record(r Record) {
	s.mu.Lock()
	s.apply(r)
	s.mu.Unlock()

	// 写盘放锁外，避免磁盘 IO 拖慢其他记账
	s.appendLine(r)
}

func (s *Store) apply(r Record) {
	if r.Timestamp == 0 {
		r.Timestamp = time.Now().Unix()
	}
	d := time.Unix(r.Timestamp, 0).Format("2006-01-02")
	st := s.days[d]
	if st == nil {
		st = &DayStat{Date: d, Models: map[string]ModelStat{}}
		s.days[d] = st
	}
	st.Requests++
	switch r.Outcome {
	case "completed":
		st.Completed++
	case "error", "aborted":
		st.Failed++
	}
	st.Prompt += r.PromptTok
	st.Completion += r.CompletionTok
	st.Cached += r.CachedTok
	st.Reasoning += r.ReasoningTok
	st.Total += r.TotalTok
	st.Credits += r.Credits

	if r.Model != "" {
		ms := st.Models[r.Model]
		ms.Requests++
		if r.Outcome != "completed" {
			ms.Failed++
		}
		ms.Total += r.TotalTok
		ms.Credits += r.Credits
		st.Models[r.Model] = ms
	}

	if r.UID != "" && r.TotalTok > 0 {
		m := s.dailyTokens[r.UID]
		if m == nil {
			m = map[string]int64{}
			s.dailyTokens[r.UID] = m
		}
		m[d] += int64(r.TotalTok)
	}

	// 总计从日桶重算，而不是增量累加 ——
	// 增量在"跨日 + 补写历史记录"场景下会算错，重算成本 O(天桶数) 可忽略。
	s.recalcTotal()

	// 环形缓冲保留最近请求
	s.records[s.ringPos] = r
	s.ringPos = (s.ringPos + 1) % len(s.records)
}

func (s *Store) recalcTotal() {
	t := DayStat{Models: map[string]ModelStat{}}
	for _, d := range s.days {
		t.Requests += d.Requests
		t.Completed += d.Completed
		t.Failed += d.Failed
		t.Prompt += d.Prompt
		t.Completion += d.Completion
		t.Cached += d.Cached
		t.Reasoning += d.Reasoning
		t.Total += d.Total
		t.Credits += d.Credits
		for k, v := range d.Models {
			m := t.Models[k]
			m.Requests += v.Requests
			m.Failed += v.Failed
			m.Total += v.Total
			m.Credits += v.Credits
			t.Models[k] = m
		}
	}
	s.total = t
}

func (s *Store) appendLine(r Record) {
	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// DailyTokens 返回某账号当日已用 token。
func (s *Store) DailyTokens(uid string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dailyTokens[uid][today()]
}

// RollDay 跨日重置日额度。
//
// 为什么池和统计都要做：日限额按"本地自然日"计算，
// 跨过 0 点就该重新计数，否则账号会被昨天的额度永久停用。
func (s *Store) RollDay() bool {
	s.mu.Lock()
	d := today()
	if s.day == d {
		s.mu.Unlock()
		return false
	}
	s.day = d
	s.dailyTokens = map[string]map[string]int64{}
	// 只保留最近 7 天的日桶，抑制文件无限增长
	keys := make([]string, 0, len(s.days))
	for k := range s.days {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for i, k := range keys {
		if i >= 7 {
			delete(s.days, k)
		}
	}
	s.mu.Unlock()
	return true
}

// Overview 返回面板概览数据。
type Overview struct {
	Today      *DayStat  `json:"today"`
	Totals     DayStat   `json:"totals"`
	Daily      []DayStat `json:"daily"`
	ActiveDays int       `json:"active_days"`
	FirstDay   string    `json:"first_day,omitempty"`
}

// Overview 取概览。days 为返回的天数。
func (s *Store) Overview(days int) *Overview {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ov := &Overview{Daily: []DayStat{}}
	if t, ok := s.days[today()]; ok {
		cp := *t
		ov.Today = &cp
	} else {
		ov.Today = &DayStat{Date: today(), Models: map[string]ModelStat{}}
	}
	ov.Totals = s.total
	keys := make([]string, 0, len(s.days))
	for k := range s.days {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for i, k := range keys {
		if i >= days {
			break
		}
		d := *s.days[k]
		if d.Models == nil {
			d.Models = map[string]ModelStat{}
		}
		ov.Daily = append(ov.Daily, d)
	}
	ov.ActiveDays = len(s.days)
	if n := len(keys); n > 0 {
		ov.FirstDay = keys[n-1]
	}
	return ov
}

// Recent 返回最近 n 条请求（最新在前）。
func (s *Store) Recent(n int) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, n)
	for i := 1; i <= len(s.records) && i <= n; i++ {
		idx := (s.ringPos - i + len(s.records)*2) % len(s.records)
		r := s.records[idx]
		if r.Timestamp == 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Range 支持按时间范围查询：today / 24h / 7d / 30d / all，或 from+to（Unix 秒）。
func (s *Store) Range(scope string, from, to int64) *Overview {
	now := time.Now()
	var start time.Time
	switch scope {
	case "today":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "24h":
		start = now.Add(-24 * time.Hour)
	case "7d":
		start = now.Add(-7 * 24 * time.Hour)
	case "30d":
		start = now.Add(-30 * 24 * time.Hour)
	case "all":
		start = time.Unix(0, 0)
	default:
		if from > 0 {
			start = time.Unix(from, 0)
		} else {
			return s.Overview(30)
		}
	}
	cutoff := start.Unix()
	if to > 0 {
		cutoff = to
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ov := &Overview{Daily: []DayStat{}}
	keys := make([]string, 0, len(s.days))
	for k := range s.days {
		if d, err := time.Parse("2006-01-02", k); err == nil {
			ts := d.Unix()
			if ts < cutoff && cutoff != 0 {
				continue
			}
			if to > 0 && ts > to {
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	for _, k := range keys {
		d := *s.days[k]
		if d.Models == nil {
			d.Models = map[string]ModelStat{}
		}
		ov.Daily = append(ov.Daily, d)
		ov.Totals.Requests += d.Requests
		ov.Totals.Completed += d.Completed
		ov.Totals.Failed += d.Failed
		ov.Totals.Prompt += d.Prompt
		ov.Totals.Completion += d.Completion
		ov.Totals.Cached += d.Cached
		ov.Totals.Total += d.Total
		ov.Totals.Credits += d.Credits
		ov.ActiveDays++
		if ov.FirstDay == "" || k < ov.FirstDay {
			ov.FirstDay = k
		}
	}
	if len(keys) > 0 {
		ov.Today = &DayStat{Date: keys[0], Models: map[string]ModelStat{}}
		if t, ok := s.days[keys[0]]; ok {
			cp := *t
			ov.Today = &cp
		}
	}
	return ov
}
