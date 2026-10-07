package proxy

import "strings"

// ModelInfo 是一个对外暴露的模型描述。
//
// 字段刻意比 OpenAI 规范多：客户端（如 Cherry Studio、LobeChat）
// 会读 various 扩展字段来渲染能力标签，多给几个能少一轮"为什么不支持"。
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	Created int64  `json:"created"`

	// 扩展字段
	DisplayName      string   `json:"display_name,omitempty"`
	ContextLength    int      `json:"context_length,omitempty"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
	SupportsVision   bool     `json:"supports_vision,omitempty"`
	SupportsTools    bool     `json:"supports_tools,omitempty"`
	SupportsThinking bool     `json:"supports_thinking,omitempty"`
	ReasoningEfforts []string `json:"supported_efforts,omitempty"`
	DefaultEffort    string   `json:"default_effort,omitempty"`
	CreditsRate      float64  `json:"credits_rate,omitempty"` // 积分倍率；0 = 免费
	Realm            string   `json:"realm,omitempty"`        // cn / intl
	IsDefault        bool     `json:"is_default,omitempty"`
}

// builtinModels 是内置模型表。
//
// 为什么要内置表而不是每次都问上游：
// 上游目录接口偶发不可用，没有兜底会让客户端在启动时拿不到任何模型。
// 同时这里过滤掉了代码补全专用模型与底层专线变体（codewise-* / *-volc / *-lkeap），
// 只保留桌面端对外宣告的模型，与官方 UI 对齐。
//
// 倍率字段只写"已知事实"，未知的留 0（免费）而不是猜一个值——
// 猜错会让面板显示的积分消耗对不上，实际上游计费。
var builtinModels = func() []any {
	raw := []ModelInfo{
		// ---- 国内版 ----
		{ID: "hy4-preview-f", DisplayName: "混元 4 预览(免费)", ContextLength: 1000000, MaxOutputTokens: 64000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", Realm: "cn", IsDefault: true},
		{ID: "hy3", DisplayName: "混元 3", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", Realm: "cn"},
		{ID: "deepseek-v4.1-flash", DisplayName: "DeepSeek V4.1 闪速", ContextLength: 128000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 0.11, Realm: "cn"},
		{ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 增强", ContextLength: 128000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 1.0, Realm: "cn"},
		{ID: "glm-5.3", DisplayName: "GLM 5.3", ContextLength: 200000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 0.6, Realm: "cn"},
		{ID: "glm-5.3-flash", DisplayName: "GLM 5.3 闪速", ContextLength: 200000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 0.06, Realm: "cn"},
		{ID: "glm-5.2", DisplayName: "GLM 5.2", ContextLength: 200000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 0.3, Realm: "cn"},
		{ID: "glm-5.1", DisplayName: "GLM 5.1", ContextLength: 200000, MaxOutputTokens: 16000, SupportsTools: true, SupportsThinking: true, Realm: "cn"},
		{ID: "glm-5v-turbo", DisplayName: "GLM 5V 多模态", ContextLength: 200000, MaxOutputTokens: 16000, SupportsVision: true, SupportsTools: true, CreditsRate: 0.5, Realm: "cn"},
		{ID: "kimi-k3-1", DisplayName: "Kimi K3.1", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 0.8, Realm: "cn"},
		{ID: "kimi-k2.8-preview", DisplayName: "Kimi K2.8 预览", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, CreditsRate: 0.77, Realm: "cn"},
		{ID: "kimi-k2.7", DisplayName: "Kimi K2.7", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, CreditsRate: 0.6, Realm: "cn"},
		{ID: "kimi-k2.6", DisplayName: "Kimi K2.6", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, CreditsRate: 0.4, Realm: "cn"},
		{ID: "minimax-m3", DisplayName: "MiniMax M3", ContextLength: 200000, MaxOutputTokens: 32000, SupportsTools: true, CreditsRate: 0.4, Realm: "cn"},

		// ---- 国际版 ----
		{ID: "gpt-6-astra", DisplayName: "GPT-6 Astra", ContextLength: 400000, MaxOutputTokens: 64000, SupportsVision: true, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh"}, DefaultEffort: "high", CreditsRate: 1.5, Realm: "intl"},
		{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", ContextLength: 400000, MaxOutputTokens: 32000, SupportsVision: true, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 1.0, Realm: "intl"},
		{ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra", ContextLength: 400000, MaxOutputTokens: 32000, SupportsVision: true, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 1.0, Realm: "intl"},
		{ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna", ContextLength: 400000, MaxOutputTokens: 32000, SupportsVision: true, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 0.8, Realm: "intl"},
		{ID: "gpt-5.5", DisplayName: "GPT-5.5", ContextLength: 400000, MaxOutputTokens: 32000, SupportsVision: true, SupportsTools: true, SupportsThinking: true, Realm: "intl"},
		{ID: "gpt-5.4", DisplayName: "GPT-5.4", ContextLength: 400000, MaxOutputTokens: 32000, SupportsVision: true, SupportsTools: true, SupportsThinking: true, Realm: "intl"},
		{ID: "deepseek-v4.1-flash", DisplayName: "DeepSeek V4.1 闪速", ContextLength: 128000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, CreditsRate: 0.11, Realm: "intl"},
		{ID: "glm-5.3", DisplayName: "GLM 5.3", ContextLength: 200000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, CreditsRate: 0.6, Realm: "intl"},
		{ID: "glm-5.2", DisplayName: "GLM 5.2", ContextLength: 200000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, CreditsRate: 0.3, Realm: "intl"},
		{ID: "gemini-3.5-flash", DisplayName: "Gemini 3.5 闪速", ContextLength: 1000000, MaxOutputTokens: 64000, SupportsVision: true, SupportsTools: true, SupportsThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}, DefaultEffort: "high", CreditsRate: 0.4, Realm: "intl"},
		{ID: "grok-4.7", DisplayName: "Grok 4.7", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, SupportsThinking: true, CreditsRate: 0.9, Realm: "intl"},
		{ID: "kimi-k3", DisplayName: "Kimi K3", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, CreditsRate: 0.8, Realm: "intl"},
		{ID: "kimi-k2.6", DisplayName: "Kimi K2.6", ContextLength: 256000, MaxOutputTokens: 32000, SupportsTools: true, CreditsRate: 0.4, Realm: "intl"},
	}
	out := make([]any, 0, len(raw))
	for _, m := range raw {
		m.Object = "model"
		m.OwnedBy = "workbuddy"
		if m.Created == 0 {
			m.Created = 1767225600 // 2026-01-01，固定值即可
		}
		cp := m
		out = append(out, cp)
	}
	return out
}

// modelsForRealm 返回某区域可用的模型列表。
//
// 同名模型可以同时存在于两个区域（各存一条，靠 Realm 字段区分），
// 因此这里按区域过滤而不是靠 ID 后缀 —— 后缀会破坏客户端的模型名匹配。
func modelsForRealm(realm string) []any {
	if realm == "" {
		return builtinModels()
	}
	var out []any
	for _, m := range builtinModels() {
		mi := m.(ModelInfo)
		if mi.Realm == realm || mi.Realm == "" {
			out = append(out, mi)
		}
	}
	return out
}

// RealmOfModel 判断一个模型 ID 在哪些区域可用。
//
// 客户端只给模型名不带区域时（OpenAI 协议没有区域概念），
// 需要知道该模型属于哪套体系，否则无法确定该用哪区的账号去请求。
// 多个区域都可用时返回第一个，这是确定性的（账号池按区域过滤）。
func RealmOfModel(model string) (string, bool) {
	for _, m := range builtinModels() {
		mi := m.(ModelInfo)
		if mi.ID == model {
			return mi.Realm, true
		}
	}
	// 不在表里的模型（用户自建别名 / 上游新发的）：按命名特征猜一个
	return guessRealmFromModel(model), false
}

// guessRealmFromModel 从模型名推断区域。
func guessRealmFromModel(model string) string {
	l := strings.ToLower(model)
	switch {
	case strings.HasPrefix(l, "gpt-"), strings.HasPrefix(l, "grok-"),
		strings.HasPrefix(l, "gemini-"), strings.HasPrefix(l, "claude"):
		return "intl"
	}
	return "cn"
}

// ModelsPayload 供面板复用：返回 OpenAI /v1/models 形状的响应体。
// 面板端点走面板自己的鉴权，避免浏览器 fetch /v1/models 没带 key 而 401。
func ModelsPayload(realm string) map[string]any {
	if realm != "" && !ValidRealmString(realm) {
		realm = ""
	}
	return map[string]any{"object": "list", "data": modelsForRealm(realm)}
}

// ValidRealmString 与 upstream.ValidRealm 等价的本地包装，避免多一处导入。
func ValidRealmString(s string) bool { return s == "cn" || s == "intl" }

// realmOrDefault 收敛区域默认值，避免到处写三元表达式。
func realmOrDefault(def, got string) string {
	if got != "" {
		return got
	}
	return def
}
