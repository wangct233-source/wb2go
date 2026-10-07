package upstream

import (
	"encoding/json"
	"sort"
	"strings"
)

// Options 是一次出站请求的构造选项。
type Options struct {
	Identity    Identity
	Fingerprint Fingerprint
	Model       string
	Realm       string
	Stream      bool // 客户端是否要流式
	Messages    []any
	Tools       []any
	ToolChoice  any
	System      string // 网关自己的系统提示词
	MaxTokens   int
	Effort      string // low / medium / high
	Thinking    bool   // 是否开启思维链
	CacheKey    string // 非空则注入 prompt_cache_key
	Sid         string // 会话 ID，用于派生缓存键
	Sanitize    bool   // 是否开启指纹脱敏
	SanitizeSys bool   // 是否对 system 做身份声明剥离
}

// PrepareBody 把客户端请求改写成上游能接受的形式。
//
// 顺序不可交换，下面每一步都依赖上一步的结果。
// 这是整个网关最需要小心的函数，改动时务必保持顺序并补测试。
func PrepareBody(in map[string]any, opt Options) map[string]any {
	out := make(map[string]any, len(in)+4)

	// ---- 1. 透传原字段，但剥离所有下划线开头的私有标记 ----
	// 客户端 SDK 常塞 _namespace_map / _web_tools 之类的私有字段，
	// 上游对未知字段会直接 400 拒收。
	for k, v := range in {
		if strings.HasPrefix(k, "_") {
			continue
		}
		out[k] = v
	}

	// ---- 2. 强制流式 ----
	// 上游只提供流式接口。非流式请求由本地聚合 SSE 得到，
	// 这样能保证两条路径看到的上游行为完全一致。
	out["stream"] = true
	so, _ := out["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	// 带上 usage，否则本地无从计费与统计
	so["include_usage"] = true
	out["stream_options"] = so

	// ---- 3. 模型名归一 ----
	if opt.Model != "" {
		out["model"] = opt.Model
	}

	// ---- 4. max_completion_tokens → max_tokens ----
	// 新版 SDK 用 max_completion_tokens，上游只认老字段名。
	if v, ok := out["max_completion_tokens"]; ok {
		if _, dup := out["max_tokens"]; !dup {
			out["max_tokens"] = v
		}
		delete(out, "max_completion_tokens")
	}
	if opt.MaxTokens > 0 {
		out["max_tokens"] = opt.MaxTokens
	}

	// ---- 5. 提取 messages 并做角色归一 ----
	msgs := extractMessages(out)
	msgs = normalizeRoles(msgs)
	out["messages"] = msgs

	// ---- 6. 工具配对自愈 ----
	// 客户端写不回工具结果时，坏历史会被每轮重放，上游对之后每条消息都返回 400，
	// 一次失败调用就能报废整条会话。并行调用之间插入的消息同样会打断配对。
	msgs = repairToolPairs(msgs)
	out["messages"] = msgs

	// ---- 7. tool_choice 归一 ----
	if tc, ok := out["tool_choice"]; ok {
		if n := normalizeToolChoice(tc); n != nil {
			out["tool_choice"] = n
		} else {
			delete(out, "tool_choice")
		}
	}
	// tool_choice=none 时保留工具声明，只靠字段表达"本轮不许调用"。
	// 早前实现在这里删掉整个 tools 声明，导致模型失去结构化工具通道后
	// 把调用降级成伪 JSON 塞进 content，Agent 客户端解析不到、反复追问，
	// 上下文每轮 +2 条消息直到撑爆窗口。
	if t, ok := out["tools"].([]any); ok {
		out["tools"] = normalizeTools(t)
	}

	// ---- 8. 注入系统提示词 ----
	if opt.System != "" {
		out["messages"] = injectSystem(out["messages"].([]any), opt.System, opt.SanitizeSys)
	}

	// ---- 9. 思维链注入 ----
	if opt.Thinking {
		out["messages"] = backfillReasoning(out["messages"].([]any), opt.Effort)
	}

	// ---- 10. 指纹脱敏 ----
	if opt.Sanitize {
		out["messages"] = sanitizeMessages(out["messages"].([]any))
	}

	// ---- 11. 提示词缓存键 ----
	if opt.CacheKey != "" {
		out["prompt_cache_key"] = opt.CacheKey
	}

	// ---- 12. effort 档位 ----
	if opt.Thinking && opt.Effort != "" {
		out["reasoning_effort"] = opt.Effort
	}
	return out
}

// extractMessages 取出消息数组并保证是 []any。
func extractMessages(m map[string]any) []any {
	switch v := m["messages"].(type) {
	case []any:
		return v
	case nil:
		return nil
	}
	// 极少数客户端把 messages 传成 JSON 字符串
	if s, ok := m["messages"].(string); ok {
		var arr []any
		if json.Unmarshal([]byte(s), &arr) == nil {
			return arr
		}
	}
	return nil
}

// normalizeRoles 把 developer 角色改成 system。
//
// 部分 SDK（Codex 系列）用 developer 角色，上游只认 system 与 user/assistant/tool。
func normalizeRoles(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "developer" {
			mm["role"] = "system"
		}
		// 剥掉下划线私有字段
		for k := range mm {
			if strings.HasPrefix(k, "_") {
				delete(mm, k)
			}
		}
		out = append(out, mm)
	}
	return out
}

// injectSystem 把网关提示词插到消息列表开头。
//
// mode 语义（由调用方决定内容，这里只负责插入位置）：
//   - custom：调用方已把 system 删干净，这里插入
//   - append：保留既有 system 块，在其后插入
func injectSystem(msgs []any, prompt string, doStrip bool) []any {
	if prompt == "" {
		return msgs
	}
	if doStrip {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if r, _ := mm["role"].(string); r == "system" {
				if s, ok := mm["content"].(string); ok {
					mm["content"] = stripIdentity(s)
				} else if parts, ok := mm["content"].([]any); ok {
					for _, p := range parts {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						if s, ok := pm["text"].(string); ok {
							pm["text"] = stripIdentity(s)
						}
					}
					mm["content"] = parts
				}
			}
		}
	}
	head := map[string]any{"role": "system", "content": prompt}
	return append([]any{head}, msgs...)
}

// sanitizeMessages 清洗全部消息里的指纹串。
func sanitizeMessages(msgs []any) []any {
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := mm["content"].(string); ok {
			if hasFingerprint(s) {
				mm["content"] = sanitizeText(s)
			}
			continue
		}
		if parts, ok := mm["content"].([]any); ok {
			for _, p := range parts {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if s, ok := pm["text"].(string); ok && hasFingerprint(s) {
					pm["text"] = sanitizeText(s)
				}
			}
			continue
		}
		// 工具调用参数同样可能含指纹串（content=null 的调用轮最容易漏）
		if tcs, ok := mm["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				tcm, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := tcm["function"].(map[string]any)
				if !ok {
					continue
				}
				if args, ok := fn["arguments"].(string); ok && hasFingerprint(args) {
					fn["arguments"] = sanitizeText(args)
				}
			}
		}
	}
	return msgs
}

// normalizeToolChoice 把各种 tool_choice 形态压成上游接受的形式。
// 上游只认字符串，对象形式会触发 11101。
func normalizeToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if f, ok := t["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok {
				return n
			}
		}
	}
	return nil
}

// normalizeTools 清洗工具声明：去掉私有字段与空函数体。
func normalizeTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			// 已经是裸 function 形态
			out = append(out, tm)
			continue
		}
		for k := range fn {
			if strings.HasPrefix(k, "_") {
				delete(fn, k)
			}
		}
		out = append(out, tm)
	}
	return out
}

// repairToolPairs 修复工具调用与结果的配对。
//
// 两个问题：
//  1. 孤儿 tool_call：客户端没写回结果 → 按同一份 id 集合对称裁剪
//  2. 顺序错位：并行调用之间插了别的消息 → 把结果块移回所属批次之后
//
// 为什么要对称裁剪：多删一个 tool_call 与少删一个 result 同样会让配对失衡，
// 所以两侧都按同一份 id 集合处理。
func repairToolPairs(msgs []any) []any {
	if len(msgs) == 0 {
		return msgs
	}
	// 收集 tool_call id → 该 id 的结果应紧随哪条 assistant 消息
	callIDs := map[string]bool{}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if tcs, ok := mm["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				if tcm, ok := tc.(map[string]any); ok {
					if id, ok := tcm["id"].(string); ok && id != "" {
						callIDs[id] = true
					}
				}
			}
		}
	}
	if len(callIDs) == 0 {
		return msgs
	}
	// 收集实际存在的结果 id
	resultIDs := map[string]bool{}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "tool" {
			if id, ok := mm["tool_call_id"].(string); ok {
				resultIDs[id] = true
			}
		}
	}
	// 对称裁剪：只保留两侧都有的 id
	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}
	if len(keep) == len(callIDs) {
		// 无孤儿调用，只需修顺序
		return reorderToolResults(msgs, keep)
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		r, _ := mm["role"].(string)
		if r == "tool" {
			if id, ok := mm["tool_call_id"].(string); ok && !keep[id] {
				continue // 孤儿结果，删
			}
			out = append(out, mm)
			continue
		}
		if tcs, ok := mm["tool_calls"].([]any); ok {
			filtered := make([]any, 0, len(tcs))
			for _, tc := range tcs {
				if tcm, ok := tc.(map[string]any); ok {
					if id, ok := tcm["id"].(string); ok && !keep[id] {
						continue // 孤儿调用，删
					}
				}
				filtered = append(filtered, tc)
			}
			if len(filtered) == 0 {
				continue // 整条 assistant 消息只剩空调用，删掉
			}
			mm["tool_calls"] = filtered
		}
		out = append(out, mm)
	}
	return reorderToolResults(out, keep)
}

// reorderToolResults 把 tool 结果块移动到发起它的那条 assistant 消息之后。
//
// 客户端（如 Codex）会在并行工具调用之间插入 notice 消息，
// 结果块被隔开后上游认为配对断裂，对之后每条消息都返回 400。
func reorderToolResults(msgs []any, keep map[string]bool) []any {
	// 找出每个 assistant 消息对应的结果块位置
	assistantIdx := map[string]int{} // tool_call_id → assistant 消息下标
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "assistant" {
			if tcs, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					if tcm, ok := tc.(map[string]any); ok {
						if id, ok := tcm["id"].(string); ok {
							assistantIdx[id] = i
						}
					}
				}
			}
		}
	}
	results := map[string]map[string]any{}
	others := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			others = append(others, m)
			continue
		}
		if r, _ := mm["role"].(string); r == "tool" {
			if id, ok := mm["tool_call_id"].(string); ok && keep[id] {
				results[id] = mm
			}
			continue
		}
		others = append(others, mm)
	}
	if len(results) == 0 {
		return others
	}
	// 组装：按 assistant 消息下标，把它的结果块插在紧随其后
	out := make([]any, 0, len(others)+len(results))
	placed := map[string]bool{}
	for _, m := range others {
		out = append(out, m)
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if r, _ := mm["role"].(string); r != "assistant" {
			continue
		}
		tcs, _ := mm["tool_calls"].([]any)
		ids := make([]string, 0, len(tcs))
		for _, tc := range tcs {
			if tcm, ok := tc.(map[string]any); ok {
				if id, ok := tcm["id"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
		sort.Strings(ids) // 稳定顺序，避免每次请求结果块次序不同
		for _, id := range ids {
			if res, ok := results[id]; ok && !placed[id] {
				out = append(out, res)
				placed[id] = true
			}
		}
	}
	// 有没挂上 assistant 的结果（理论上不该有），追加到末尾保底
	ids := make([]string, 0, len(results))
	for id := range results {
		if !placed[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, results[id])
	}
	_ = assistantIdx
	return out
}

// backfillReasoning 回填上一轮的 reasoning_content。
//
// 上游在开启思维链时要求历史里必须带上一轮的思考内容，否则返回 11155。
// 同时把 effort 档位写进 thinking 块——实测只给 thinking.type=enabled
// 并不开启推理（reasoning_tokens 为 0），必须搭配档位才有效果。
func backfillReasoning(msgs []any, effort string) []any {
	effort = normalizeEffort(effort)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "assistant" {
			// 已经有 reasoning_content 就跳过，避免覆盖真实思考内容
			if _, has := mm["reasoning_content"]; !has {
				if rc := extractReasoning(mm); rc != "" {
					mm["reasoning_content"] = rc
				}
			}
		}
	}
	return msgs
}

// normalizeEffort 归一 effort 档位。
//
// 上游支持的档位是 high / low 等，客户端可能传 minimal / xhigh / none。
// 无法识别的值统一落到 high（最保险的档位，副作用最小）。
func normalizeEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "min", "none":
		return "low"
	case "low":
		return "low"
	case "medium", "mid":
		return "medium"
	case "high", "xhigh", "max":
		return "high"
	case "":
		return "high"
	}
	return "high"
}

// extractReasoning 从 assistant 消息里抽出思考内容（兼容 reasoning / thinking 两种形态）。
func extractReasoning(m map[string]any) string {
	if s, ok := m["reasoning_content"].(string); ok && s != "" {
		return s
	}
	if s, ok := m["reasoning"].(string); ok && s != "" {
		return s
	}
	if parts, ok := m["reasoning"].([]any); ok {
		var b strings.Builder
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// EffortForModel 从客户端请求里解析思考档位。
func EffortForModel(in map[string]any) string {
	if s, ok := in["reasoning_effort"].(string); ok && s != "" {
		return s
	}
	if r, ok := in["reasoning"].(map[string]any); ok {
		if s, ok := r["effort"].(string); ok && s != "" {
			return s
		}
		if t, ok := r["type"].(string); ok && t == "disabled" {
			return "none"
		}
	}
	if t, ok := in["thinking"].(map[string]any); ok {
		if tp, ok := t["type"].(string); ok {
			if tp == "disabled" {
				return "none"
			}
			return "high" // 开启但没给档位
		}
	}
	return ""
}

// ThinkingEnabled 判断客户端是否要求开启思维链。
//
// 只要给了任何非关闭类的 effort 档位，就算要求思考 ——
// 即使请求里没有 thinking 块（Codex 只发 reasoning_effort 是常见形态）。
func ThinkingEnabled(in map[string]any) bool {
	if s, ok := in["reasoning_effort"].(string); ok && s != "" && s != "none" {
		return true
	}
	if r, ok := in["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok && e != "" && e != "none" {
			return true
		}
	}
	if r, ok := in["reasoning"].(map[string]any); ok {
		if t, _ := r["type"].(string); t == "enabled" || t == "adaptive" {
			return true
		}
	}
	if t, ok := in["thinking"].(map[string]any); ok {
		if tp, _ := t["type"].(string); tp == "enabled" || tp == "adaptive" {
			return true
		}
	}
	return false
}
