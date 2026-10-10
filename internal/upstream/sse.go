package upstream

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ChatChunk 是上游 SSE 的一帧（已归一为 OpenAI 形态）。
type ChatChunk struct {
	ID               string
	Object           string
	Created          int64
	Model            string
	Choices          []Choice
	Usage            *Usage
	ReasoningContent string
}

// Choice 是一组候选输出。
type Choice struct {
	Index        int
	Delta        Delta
	FinishReason string
}

// Delta 是增量内容。
type Delta struct {
	Role             string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	// Raw 保留上游原始 delta，用于透传少数字段
	Raw map[string]any
}

// ToolCall 是工具调用增量。
type ToolCall struct {
	Index int
	ID    string
	Type  string
	Name  string
	Args  string
}

// Usage 是 token 用量。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
}

func (u *Usage) empty() bool {
	return u == nil || (u.PromptTokens == 0 && u.CompletionTokens == 0)
}

// SSE 解析器。
//
// 逐行增量解析，不整帧缓冲——上游可能一次吐几 MB，
// 整帧读完再处理会让首字节延迟等于整帧长度。
type SSEParser struct {
	sc  *bufio.Scanner
	max int
}

// NewSSEParser 建解析器。maxLine 控制单帧上限，默认 8MB。
func NewSSEParser(r io.Reader) *SSEParser {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	return &SSEParser{sc: sc, max: 8 << 20}
}

// Next 返回下一帧的数据部分（已剥掉 "data:" 前缀）。
// 心跳、注释、[DONE] 返回 ok=false。
func (p *SSEParser) Next() ([]byte, bool, error) {
	for p.sc.Scan() {
		line := p.sc.Text()
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE 注释 / 心跳
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(line[5:])
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				return nil, false, nil
			}
			return []byte(data), true, nil
		}
		// event: / id: 等字段行跳过
	}
	if err := p.sc.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, io.EOF
}

// ToChunk 把一帧原始 JSON 转成 ChatChunk。
func ToChunk(raw []byte) (*ChatChunk, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("上游帧不是合法 JSON: %w", err)
	}
	// 错误帧
	if errObj, ok := m["error"]; ok {
		return nil, fmt.Errorf("上游返回错误: %s", compact(errObj))
	}
	ch := &ChatChunk{}
	ch.ID, _ = m["id"].(string)
	ch.Object, _ = m["object"].(string)
	ch.Model, _ = m["model"].(string)
	if v, ok := m["created"].(float64); ok {
		ch.Created = int64(v)
	}

	if u, ok := m["usage"].(map[string]any); ok {
		usage := &Usage{}
		usage.PromptTokens = intOf(u["prompt_tokens"])
		usage.CompletionTokens = intOf(u["completion_tokens"])
		usage.TotalTokens = intOf(u["total_tokens"])
		usage.CachedTokens = intOf(u["cached_tokens"])
		usage.ReasoningTokens = intOf(u["reasoning_tokens"])
		if !usage.empty() {
			ch.Usage = usage
		}
	}

	choices, _ := m["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		chc := Choice{}
		if v, ok := cm["index"].(float64); ok {
			chc.Index = int(v)
		}
		chc.FinishReason, _ = cm["finish_reason"].(string)
		if d, ok := cm["delta"].(map[string]any); ok {
			chc.Delta = parseDelta(d)
		}
		ch.Choices = append(ch.Choices, chc)
	}
	return ch, nil
}

func parseDelta(d map[string]any) Delta {
	out := Delta{Raw: d}
	out.Role, _ = d["role"].(string)
	out.Content, _ = d["content"].(string)
	// 思维链在不同上游版本里出现过三种字段名
	for _, k := range []string{"reasoning_content", "reasoning", "thinking"} {
		if s, ok := d[k].(string); ok && s != "" {
			out.ReasoningContent = s
			break
		}
	}
	tcs, _ := d["tool_calls"].([]any)
	for i, tc := range tcs {
		tcm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		var call ToolCall
		call.Index = i
		if v, ok := tcm["index"].(float64); ok {
			call.Index = int(v)
		}
		call.ID, _ = tcm["id"].(string)
		call.Type, _ = tcm["type"].(string)
		if fn, ok := tcm["function"].(map[string]any); ok {
			call.Name, _ = fn["name"].(string)
			// 参数分片到达，靠 index 拼接
			if a, ok := fn["arguments"].(string); ok {
				call.Args = a
			}
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	return out
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	}
	return 0
}

// Writer 负责按 OpenAI SSE 规范向客户端写帧。
type Writer struct {
	w        io.Writer
	flush    func()
	Model    string
	ID       string
	Created  int64
	usage    *Usage
	roleSent bool
	sawDone  bool
	err      error
}

// NewWriter 建写器。
func NewWriter(w io.Writer, flush func(), model string) *Writer {
	if flush == nil {
		flush = func() {}
	}
	return &Writer{w: w, flush: flush, Model: model, ID: "chatcmpl-" + randomHex(12), Created: nowMillis() / 1000}
}

// WriteChunk 把上游一帧透传给客户端（只保留规范字段）。
func (wr *Writer) WriteChunk(ch *ChatChunk) {
	if ch.Usage != nil {
		wr.usage = ch.Usage
	}
	for _, c := range ch.Choices {
		frame := map[string]any{
			"id": wr.ID, "object": "chat.completion.chunk",
			"created": wr.Created, "model": wr.Model,
			"choices": []any{map[string]any{
				"index": c.Index,
				"delta": deltaToMap(c.Delta),
			}},
		}
		wr.writeFrame(frame)
	}
}

// WriteError 写一个错误帧。上游中途报错时客户端需要收到明确的错误，
// 而不是干等到连接结束。
func (wr *Writer) WriteError(code, msg string) {
	wr.writeFrame(map[string]any{
		"error": map[string]any{"message": msg, "type": "upstream_error", "code": code},
	})
}

// Done 写结束帧。
//
// 保证恰好一个 [DONE]：上游漏发时兜底补写，
// 否则客户端会一直等下去。
func (wr *Writer) Done() {
	if wr.sawDone {
		return
	}
	wr.sawDone = true
	// 最后一帧带上 usage，客户端据此计费
	if wr.usage != nil {
		frame := map[string]any{
			"id": wr.ID, "object": "chat.completion.chunk",
			"created": wr.Created, "model": wr.Model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": wr.usage.PromptTokens, "completion_tokens": wr.usage.CompletionTokens,
				"total_tokens": wr.usage.TotalTokens,
			},
		}
		if wr.usage.CachedTokens > 0 {
			frame["usage"].(map[string]any)["cached_tokens"] = wr.usage.CachedTokens
		}
		if wr.usage.ReasoningTokens > 0 {
			frame["usage"].(map[string]any)["reasoning_tokens"] = wr.usage.ReasoningTokens
		}
		wr.writeFrame(frame)
	}
	if wr.err == nil {
		_, wr.err = io.WriteString(wr.w, "data: [DONE]\n\n")
		wr.flush()
	}
}

// Err 返回写入过程中遇到的客户端侧错误（客户端断开时非空）。
func (wr *Writer) Err() error { return wr.err }

func (wr *Writer) writeFrame(v any) {
	if wr.err != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	if _, wr.err = io.WriteString(wr.w, "data: "+string(data)+"\n\n"); wr.err == nil {
		wr.flush()
	}
}

func deltaToMap(d Delta) map[string]any {
	m := map[string]any{}
	// role 只在第一帧发一次
	if d.Role != "" {
		m["role"] = d.Role
	}
	if d.Content != "" {
		m["content"] = d.Content
	}
	if d.ReasoningContent != "" {
		m["reasoning_content"] = d.ReasoningContent
	}
	if len(d.ToolCalls) > 0 {
		arr := make([]any, 0, len(d.ToolCalls))
		for _, tc := range d.ToolCalls {
			fn := map[string]any{}
			if tc.Name != "" {
				fn["name"] = tc.Name
			}
			if tc.Args != "" {
				fn["arguments"] = tc.Args
			}
			it := map[string]any{"index": tc.Index}
			if tc.ID != "" {
				it["id"] = tc.ID
			}
			if tc.Type != "" {
				it["type"] = tc.Type
			}
			if len(fn) > 0 {
				it["function"] = fn
			}
			arr = append(arr, it)
		}
		m["tool_calls"] = arr
	}
	return m
}

// Aggregator 把 SSE 流聚合成一个完整的 chat.completion 响应（非流式路径）。
type Aggregator struct {
	content   strings.Builder
	reasoning strings.Builder
	toolCalls map[int]*aggTool
	order     []int
	finish    string
	usage     *Usage
	model     string
}

type aggTool struct {
	id, typ, name string
	args          strings.Builder
}

// NewAggregator 建聚合器。
func NewAggregator() *Aggregator {
	return &Aggregator{toolCalls: map[int]*aggTool{}}
}

// Usage 返回流中最后一帧携带的 token 用量。
// 上游只在流末尾发 usage 帧；没有则为 nil。
func (a *Aggregator) Usage() *Usage { return a.usage }

// Add 累加一帧。
func (a *Aggregator) Add(ch *ChatChunk) {
	if ch.Usage != nil {
		a.usage = ch.Usage
	}
	if ch.Model != "" {
		a.model = ch.Model
	}
	for _, c := range ch.Choices {
		a.content.WriteString(c.Delta.Content)
		a.reasoning.WriteString(c.Delta.ReasoningContent)
		if c.FinishReason != "" {
			a.finish = c.FinishReason
		}
		for _, tc := range c.Delta.ToolCalls {
			t, ok := a.toolCalls[tc.Index]
			if !ok {
				t = &aggTool{}
				if tc.ID != "" {
					t.id = tc.ID
				}
				t.typ = orDefault(tc.Type, "function")
				t.name = tc.Name
				a.toolCalls[tc.Index] = t
				a.order = append(a.order, tc.Index)
			}
			// 名称可能分片到达
			if tc.Name != "" {
				t.name = tc.Name
			}
			t.args.WriteString(tc.Args)
		}
	}
}

// Result 产出完整的非流式响应体。
func (a *Aggregator) Result(model string) map[string]any {
	msg := map[string]any{"role": "assistant", "content": a.content.String()}
	if rc := a.reasoning.String(); rc != "" {
		msg["reasoning_content"] = rc
	}
	if len(a.order) > 0 {
		arr := make([]any, 0, len(a.order))
		for _, idx := range a.order {
			t := a.toolCalls[idx]
			args := t.args.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			arr = append(arr, map[string]any{
				"id": t.id, "type": t.typ,
				"function": map[string]any{"name": t.name, "arguments": args},
			})
		}
		msg["tool_calls"] = arr
	}
	finish := a.finish
	if finish == "" {
		finish = orDefault(finishFor(a.finish), "stop")
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + randomHex(12),
		"object":  "chat.completion",
		"created": nowMillis() / 1000,
		"model":   orDefault(model, a.model),
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
	}
	if a.usage != nil {
		resp["usage"] = map[string]any{
			"prompt_tokens":     a.usage.PromptTokens,
			"completion_tokens": a.usage.CompletionTokens,
			"total_tokens":      a.usage.TotalTokens,
		}
		if a.usage.CachedTokens > 0 {
			resp["usage"].(map[string]any)["cached_tokens"] = a.usage.CachedTokens
		}
		if a.usage.ReasoningTokens > 0 {
			resp["usage"].(map[string]any)["reasoning_tokens"] = a.usage.ReasoningTokens
		}
	} else {
		// 上游没给 usage 时本地估算，否则客户端拿不到计费依据
		p := estimateTokens(a.content.String())
		c := estimateTokens(a.reasoning.String())
		resp["usage"] = map[string]any{
			"prompt_tokens": 0, "completion_tokens": c, "total_tokens": c,
		}
		_ = p
	}
	return resp
}

// Finish 返回聚合到的 finish_reason。
func (a *Aggregator) Finish() string { return a.finish }

func finishFor(s string) string { return s }

// estimateTokens 粗略估算 token 数（无分词器时按字符启发式）。
//
// 局限要说明：这个估算明显不准，仅用于"上游没给 usage"时的兜底，
// 让客户端不至于完全没有计费依据。有 usage 时一律用上游的值。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	// 中英混排大致是 2 字符 ≈ 1 token
	return (len(s) + 1) / 2
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > 200 {
		return string(b[:200]) + "…"
	}
	return string(b)
}

// randomHex 生成随机十六进制串（用于响应 ID / 请求 ID）。
// 用 crypto/rand 而不是自制的线性同余：响应 ID 会出现在日志里做关联键，
// 可预测的 ID 会让第三方能推断出请求计数。
func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下（系统随机源不可用）退回时间派生，
		// 此时响应 ID 唯一性会变弱，但请求仍能正常完成。
		binary.BigEndian.PutUint64(b, uint64(nowMillis()))
	}
	return hex.EncodeToString(b)[:n]
}
