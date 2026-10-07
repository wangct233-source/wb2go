package upstream

import (
	"strings"
	"testing"
)

// TestClassifyPriority 验证错误分类的判定优先级。
//
// 顺序有讲究：余额耗尽与会话失效都可能返回非 2xx。
// 若先按状态码兜底判，会把它们误判成普通故障，处置方式就错了
// （比如把"余额耗尽"当成 5xx 去喂熔断计数，而不是硬冷却到次日）。
func TestClassifyPriority(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrorKind
	}{
		{"余额耗尽优先于状态码", 402, `{"code":1001,"msg":"余额不足"}`, KindCredits},
		{"余额关键词覆盖 429", 429, `{"msg":"余额不足，请充值"}`, KindCredits},
		{"会话失效", 401, `{"msg":"Offline user session not found"}`, KindSessionDead},
		{"12153 数值码", 401, `{"code":12153,"msg":"会话已失效"}`, KindSessionDead},
		{"内容审核", 400, `{"msg":"blocked by security policy"}`, KindContentBlock},
		{"WAF 拦截", 403, `{"code":11128,"msg":"Illegal API invocation"}`, KindWAF},
		{"普通限流", 429, `{"code":1002,"msg":"rate limit exceeded"}`, KindSoftRate},
		{"限流文案不带状态码", 200, `{"msg":"请求过于频繁，请稍后"}`, KindSoftRate},
		{"404", 404, `not found`, KindNotFound},
		{"400 请求体错误", 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, KindBadRequest},
		{"5xx", 503, `service unavailable`, KindServer},
		{"正常 200", 200, `{"choices":[]}`, KindOK},
	}
	for _, tc := range cases {
		got := Classify(tc.status, []byte(tc.body), "m")
		if got.Kind != tc.want {
			t.Errorf("%s: Classify(%d, %q).Kind = %v，期望 %v",
				tc.name, tc.status, tc.body, got.Kind, tc.want)
		}
	}
}

// TestClassifyModelRateIsNotAccountRate 验证 6004 被识别为模型级限流。
//
// 上游 code 6004 的语义是"该模型用量超限"，不是账号整体被限流。
// 若按账号级冷却处理，会让"换个模型就能用"的号被整体摘出池子。
func TestClassifyModelRateIsNotAccountRate(t *testing.T) {
	body := `{"code":6004,"msg":"model deepseek-v4 usage will reset at 2026-09-19 18:29:03 UTC+8"}`
	e := Classify(429, []byte(body), "deepseek-v4")
	if e.Kind != KindModelRate {
		t.Fatalf("code 6004 应判为模型级限流，实际 %v", e.Kind)
	}
	if e.Raw != "6004" {
		t.Errorf("应提取到业务码 6004，实际 %q", e.Raw)
	}
	if e.ResetAt == 0 {
		t.Error("应从文案里解析出重置墙钟时间")
	}
	if e.Model != "deepseek-v4" {
		t.Errorf("应记录触发限流的模型，实际 %q", e.Model)
	}
	if !e.IsRateLimit() {
		t.Error("模型级限流也属限流类")
	}
}

// TestSanitizeRemovesFrameworkFingerprint 验证框架指纹被清洗。
//
// 上游内容审核按逐字精确匹配识别框架指纹串并拒流，
// 因此这些串必须在出站前消失。
func TestSanitizeRemovesFrameworkFingerprint(t *testing.T) {
	cases := []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"请参考 x-anthropic-billing-header:abc123; 继续",
		"Sisyphus-Junior - Focused executor from OhMyOpenCode",
	}
	for _, in := range cases {
		out := sanitizeText(in)
		if strings.Contains(out, "Anthropic's official CLI") {
			t.Errorf("未清除框架身份声明: %q", out)
		}
		if strings.Contains(out, "x-anthropic-billing-header:") {
			t.Errorf("未清除 billing 头串: %q", out)
		}
		if strings.Contains(out, "from OhMyOpenCode") {
			t.Errorf("未清除子代理归属短语: %q", out)
		}
	}
}

// TestSanitizeLeavesCleanTextAlone 验证无指纹文本原样保留。
//
// 过度清洗会破坏用户的正常内容 —— 这是脱敏功能最需要防的副作用。
func TestSanitizeLeavesCleanTextAlone(t *testing.T) {
	clean := []string{
		"帮我写一个 Go 的 HTTP 服务器",
		"What is the capital of France?",
		"请解释这段代码的作用：func main() {}",
		"",
	}
	for _, in := range clean {
		if got := sanitizeText(in); got != in {
			t.Errorf("干净文本被改动了：输入 %q，输出 %q", in, got)
		}
	}
}

// TestStripIdentityKeepsObligations 验证身份剥离不误删合规声明。
//
// 这是精细度要求最高的一处：既要删掉"我是 Claude Code"这类身份声明，
// 又必须保留"You are required to refuse harmful requests"这类义务句 ——
// 后者删掉会让模型的合规行为失效。
func TestStripIdentityKeepsObligations(t *testing.T) {
	in := "You are Claude Code, a CLI tool. You are required to refuse harmful requests. Please help with code."
	out := stripIdentity(in)
	if strings.Contains(out, "You are Claude Code") {
		t.Errorf("身份声明应被删: %q", out)
	}
	if !strings.Contains(out, "required to refuse") {
		t.Errorf("义务句必须保留: %q", out)
	}
	if !strings.Contains(out, "Please help with code") {
		t.Errorf("用户指令必须保留: %q", out)
	}
}

// TestStripIdentityRemovesPlainDeclaration 验证纯身份声明被删。
//
// "You are a helpful assistant" 本身就是一句身份声明（"你是助手"），
// 同样会被删 —— 这符合设计意图：上游审核认的是"声明自己是什么角色"，
// 而不只是产品名。
func TestStripIdentityRemovesPlainDeclaration(t *testing.T) {
	in := "You are ChatGPT. You are a helpful assistant."
	out := stripIdentity(in)
	if strings.Contains(out, "You are ChatGPT") {
		t.Errorf("身份声明应被删: %q", out)
	}
	if strings.Contains(out, "You are a helpful assistant") {
		t.Errorf("角色声明同样应被删: %q", out)
	}

	// 但用户的业务指令必须原样保留
	in2 := "You are ChatGPT. Please summarize the following text."
	out2 := stripIdentity(in2)
	if strings.Contains(out2, "You are ChatGPT") {
		t.Errorf("身份声明应被删: %q", out2)
	}
	if !strings.Contains(out2, "Please summarize") {
		t.Errorf("业务指令必须保留: %q", out2)
	}
}

// TestPrepareBodyForcesStream 验证出站强制流式。
//
// 上游只提供流式接口。非流式由本地聚合得到，
// 这样两条路径看到的上游行为完全一致。
func TestPrepareBodyForcesStream(t *testing.T) {
	in := map[string]any{
		"model":    "glm-5.2",
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out := PrepareBody(in, Options{Model: "glm-5.2"})
	if out["stream"] != true {
		t.Error("出站必须强制 stream:true")
	}
	so, _ := out["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Error("必须带上 include_usage，否则本地无从计费")
	}
}

// TestPrepareBodyStripsPrivateFields 验证私有字段被剥离。
//
// 客户端 SDK 常塞 _namespace_map 之类的私有字段，
// 上游对未知字段会直接 400 拒收。
func TestPrepareBodyStripsPrivateFields(t *testing.T) {
	in := map[string]any{
		"model":          "glm-5.2",
		"_web_tools":     map[string]any{"x": 1},
		"_namespace_map": []any{1, 2},
		"messages": []any{map[string]any{
			"role": "developer", "content": "sys", "_private": true,
		}},
	}
	out := PrepareBody(in, Options{Model: "glm-5.2"})
	if _, bad := out["_web_tools"]; bad {
		t.Error("顶层私有字段未剥离")
	}
	if _, bad := out["_namespace_map"]; bad {
		t.Error("顶层私有字段未剥离")
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("消息列表为空")
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" {
		t.Errorf("developer 角色应归一为 system，实际 %v", m0["role"])
	}
	if _, bad := m0["_private"]; bad {
		t.Error("消息内的私有字段未剥离")
	}
}

// TestToolChoiceNoneKeepsToolDeclarations 验证 tool_choice=none 时保留工具声明。
//
// 早前实现在这里删掉整个 tools 声明，导致模型失去结构化通道后
// 把调用降级成伪 JSON 塞进 content，Agent 客户端解析不到、
// 反复追问，上下文每轮 +2 条消息直到撑爆窗口。
func TestToolChoiceNoneKeepsToolDeclarations(t *testing.T) {
	in := map[string]any{
		"model":       "glm-5.2",
		"tool_choice": "none",
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "get_weather", "parameters": map[string]any{}},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out := PrepareBody(in, Options{Model: "glm-5.2"})
	tools, _ := out["tools"].([]any)
	if len(tools) == 0 {
		t.Error("tool_choice=none 时不应删除工具声明")
	}
	if out["tool_choice"] != "none" {
		t.Errorf("tool_choice 应保持 none，实际 %v", out["tool_choice"])
	}
}

// TestRepairToolPairsDropsOrphans 验证孤儿工具调用被对称裁剪。
//
// 客户端没写回工具结果时，坏历史会被每轮重放，上游对之后每条消息都返回 400，
// 一次失败调用就能报废整条会话。
func TestRepairToolPairsDropsOrphans(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "查天气"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "a", "arguments": `{}`}},
				map[string]any{"id": "call_2", "type": "function",
					"function": map[string]any{"name": "b", "arguments": `{}`}},
			},
		},
		// 只写回了 call_1 的结果，call_2 成了孤儿
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
		map[string]any{"role": "assistant", "content": "今天晴天"},
	}
	out := repairToolPairs(msgs)
	// call_2 应被从 assistant 的 tool_calls 里删掉
	for _, m := range out {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if tcs, has := mm["tool_calls"].([]any); has {
			for _, tc := range tcs {
				tcm := tc.(map[string]any)
				if tcm["id"] == "call_2" {
					t.Error("孤儿工具调用 call_2 未被裁剪")
				}
			}
		}
	}
}

// TestToolResultReorderedToFollowAssistant 验证结果块被移回所属批次之后。
//
// 客户端会在并行工具调用之间插入 notice 消息，
// 结果块被隔开后上游认为配对断裂，对之后每条消息都返回 400。
func TestToolResultReorderedToFollowAssistant(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "a", "arguments": `{}`}}},
		},
		// 中间插入了干扰消息
		map[string]any{"role": "user", "content": "<image_resize_notice>"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"},
	}
	out := repairToolPairs(msgs)
	// tool 结果应紧跟发起它的那条 assistant
	toolIdx, assistantIdx := -1, -1
	for i, m := range out {
		mm := m.(map[string]any)
		r, _ := mm["role"].(string)
		if r == "tool" && mm["tool_call_id"] == "c1" && toolIdx < 0 {
			toolIdx = i
		}
		if r == "assistant" && assistantIdx < 0 {
			assistantIdx = i
		}
	}
	if toolIdx != assistantIdx+1 {
		t.Errorf("工具结果应紧跟 assistant（idx %d），实际在 %d", assistantIdx, toolIdx)
	}
}

// TestCacheKeyIsolatesByAccount 验证缓存键的账号隔离。
//
// 上游缓存按账号隔离。共享 key 会让一个账号命中另一个账号的前缀缓存
// —— 那等于把对话内容泄露给第三方。
func TestCacheKeyIsolatesByAccount(t *testing.T) {
	id, _ := BuildIdentity("workbuddy", "5.5.6", "2.137.1", "")
	fpA := NewFingerprint("account-aaa", id)
	fpB := NewFingerprint("account-bbb", id)

	kA := fpA.CacheKey("session-1")
	kB := fpB.CacheKey("session-1")
	if kA == kB {
		t.Error("不同账号在同一会话下必须产生不同的缓存键")
	}
	// 同账号同会话应稳定
	if fpA.CacheKey("session-1") != fpA.CacheKey("session-1") {
		t.Error("同账号同会话的缓存键应稳定")
	}
	// 同账号不同会话应不同
	if kA == fpA.CacheKey("session-2") {
		t.Error("不同会话应产生不同的缓存键")
	}
	if !strings.HasPrefix(kA, "wb2go-") {
		t.Errorf("缓存键应有可识别前缀，实际 %q", kA)
	}
}

// TestFingerprintIsStablePerAccount 验证设备指纹的稳定性。
//
// 同一账号每次出站都应来自"同一台虚拟设备"。
// 随机 UUID 做不到这点 —— 每请求一个新设备号在服务端看来是异常行为。
func TestFingerprintIsStablePerAccount(t *testing.T) {
	id, _ := BuildIdentity("workbuddy", "5.5.6", "2.137.1", "")
	a1 := NewFingerprint("same-uid", id)
	a2 := NewFingerprint("same-uid", id)
	if a1.MachineID != a2.MachineID || a1.SessionID != a2.SessionID {
		t.Error("同账号的 machineId / sessionId 必须恒定")
	}
	b := NewFingerprint("other-uid", id)
	if a1.MachineID == b.MachineID {
		t.Error("不同账号的 machineId 必须不同（阻断跨账号关联）")
	}
}

// TestIdentityResolvePerRealm 验证身份按区域解析端点。
func TestIdentityResolvePerRealm(t *testing.T) {
	id, _ := BuildIdentity("workbuddy", "5.5.6", "2.137.1", "")
	cn := id.Resolve("cn")
	if cn.ChatBase != CNChatBase {
		t.Errorf("国内版桌面端应走 %s，实际 %s", CNChatBase, cn.ChatBase)
	}
	if cn.BillingBase != CNBillingBase {
		t.Errorf("国内版签到应走 %s，实际 %s", CNBillingBase, cn.BillingBase)
	}
	intl := id.Resolve("intl")
	if !strings.Contains(intl.ChatBase, "workbuddy.ai") {
		t.Errorf("国际版应走 workbuddy.ai，实际 %s", intl.ChatBase)
	}
	// VSCode 身份在国内版走不同域名，这是官方行为
	vs, _ := BuildIdentity("vscode", "4.9.29177644", "", "")
	if got := vs.Resolve("cn").ChatBase; got != CNWebBase {
		t.Errorf("国内版 VSCode 插件应走 %s，实际 %s", CNWebBase, got)
	}
}

// TestBuildIdentityRejectsUnknown 验证未知身份被拒。
func TestBuildIdentityRejectsUnknown(t *testing.T) {
	if _, err := BuildIdentity("bogus", "", "", ""); err == nil {
		t.Error("未知身份应返回错误")
	}
}

// TestNormalizeEffortMapsVariants 验证 effort 档位归一。
func TestNormalizeEffortMapsVariants(t *testing.T) {
	cases := map[string]string{
		"":        "high",
		"minimal": "low",
		"none":    "low",
		"LOW":     "low",
		"medium":  "medium",
		"xhigh":   "high",
		"unheard": "high", // 未知值落到最保险的档位
	}
	for in, want := range cases {
		if got := normalizeEffort(in); got != want {
			t.Errorf("normalizeEffort(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestEffortAndThinkingDetection 验证思考参数识别。
func TestEffortAndThinkingDetection(t *testing.T) {
	// Responses 形态
	in := map[string]any{"reasoning": map[string]any{"effort": "high"}}
	if EffortForModel(in) != "high" {
		t.Errorf("应从 reasoning.effort 读到 high，实际 %q", EffortForModel(in))
	}
	if !ThinkingEnabled(in) {
		t.Error("带 effort 应视为开启思考")
	}
	// Anthropic 形态
	in2 := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	if !ThinkingEnabled(in2) {
		t.Error("thinking.type=enabled 应识别为开启思考")
	}
	// 显式关闭
	in3 := map[string]any{"thinking": map[string]any{"type": "disabled"}}
	if ThinkingEnabled(in3) {
		t.Error("thinking.type=disabled 不应视为开启思考")
	}
	if EffortForModel(in3) != "none" {
		t.Errorf("disabled 应映射为 none，实际 %q", EffortForModel(in3))
	}
	// 客户端未声明
	if ThinkingEnabled(map[string]any{}) {
		t.Error("未声明时应返回 false")
	}
}

// TestZeroWidthMaskIsNoOpByDefault 验证零宽脱敏默认不改内容。
func TestZeroWidthMaskIsNoOpByDefault(t *testing.T) {
	in := "帮我分析这段代码"
	if got := ZeroWidthMask(in); got != in {
		t.Errorf("零宽脱敏不应改动普通文本: %q", got)
	}
}
