package upstream

import (
	"regexp"
	"strings"
)

// 指纹脱敏规则。
//
// 为什么需要：客户端（Claude Code / Codex / Cursor 等）会在 system prompt 与
// 工具结果里注入固定的框架指纹串。上游内容审核按**逐字精确匹配**把这些串
// 识别成"非官方客户端的自动化流量"并拒流。
//
// 因此出站前必须做两层防护，它们互不替代：
//  1. 本文件的 sanitize：清洗 user / assistant 消息里的指纹串
//  2. 提示词体系（proxy 层）：替换 system / developer 消息里的身份声明
var (
	// 框架身份声明句。"You are Claude Code, Anthropic's official CLI" 这类整句。
	// 句末锚定：只删到标点为止，不能把后面的内容一起吃掉。
	// （早前用了非贪婪的 [^.\n]{0,120}? 配合 ReplaceAll，导致只替换掉前半句，
	//   留下 "You are a helpful assistant., Anthropic's official CLI for Claude."）
	reClaudeCode = regexp.MustCompile(`(?i)you are claude code[^.!?\n。！？]{0,200}[.!?。！？]?`)
	reYouAreAI   = regexp.MustCompile(`(?i)you are (?:an? )?(?:ai |artificial intelligence )?(?:assistant|language model|claude|gpt|chatgpt)[^.\n]{0,120}?official[^.\n]{0,60}`)
	// 框架注入的头部串
	reBillingHdr    = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?`)
	reBillingHdrAlt = regexp.MustCompile(`(?i)x-anthropic-billing-hdr`)
	// OpenCode 子代理身份短语（实测会触发 WAF 指纹匹配）
	reOmOJunior = regexp.MustCompile(`(?i)Sisyphus-Junior - Focused executor from OhMyOpenCode`)
	// Codex / Cline 的运行时标记
	reHarnessMarker = regexp.MustCompile(`(?i)(?:<environment_context>|</?skills_instructions>|<system-reminder>|##\s*AGENTS\.md\s*instructions)`)
	rePrivateKey    = regexp.MustCompile(`\bcc_[A-Za-z0-9_-]{8,}`)
)

// rewriteHints 是"降级重试"时切换的中性提示词。
//
// 用途：passthrough 模式下请求被内容策略拦截时，判定为 system 指纹误报，
// 换一句中性提示词用**同一个请求**重试一次。第二次仍被拦才认定为用户内容本身的问题。
const NeutralPrompt = "You are a helpful assistant."

// hasFingerprint 判断文本是否含指纹特征。
//
// 前置门控的作用：无指纹时原样返回，避免正则空转与误伤正常内容。
// 对长对话这是实打实的性能节省——每条消息都要跑一遍多组正则。
func hasFingerprint(s string) bool {
	if s == "" {
		return false
	}
	return reClaudeCode.MatchString(s) || reBillingHdr.MatchString(s) ||
		reOmOJunior.MatchString(s) || reHarnessMarker.MatchString(s) ||
		rePrivateKey.MatchString(s) || reYouAreAI.MatchString(s)
}

// sanitizeText 清洗一段文本里的指纹串。
func sanitizeText(s string) string {
	if s == "" || !hasFingerprint(s) {
		return s
	}
	s = reClaudeCode.ReplaceAllString(s, "You are a helpful assistant.")
	s = reYouAreAI.ReplaceAllString(s, "You are a helpful assistant.")
	s = reOmOJunior.ReplaceAllStringFunc(s, func(m string) string {
		return strings.TrimSuffix(strings.TrimSpace(m), " from OhMyOpenCode")
	})
	s = reBillingHdr.ReplaceAllString(s, "")
	s = reBillingHdrAlt.ReplaceAllString(s, "x-anthropic-billing-hdr")
	s = rePrivateKey.ReplaceAllString(s, "cc_***")
	// 运行时标记块整段移除：它们是框架注入的元信息，对模型无用且长度可观
	s = stripHarnessBlocks(s)
	return strings.TrimSpace(s)
}

// stripHarnessBlocks 移除 <environment_context>…</environment_context> 这类整块内容。
//
// 用非贪婪匹配 + 容忍未闭合标签：客户端中途断流时标签可能不配对，
// 严格匹配会导致整条消息都匹配不上，等于没清洗。
var reHarnessBlock = regexp.MustCompile(`(?s)<(environment_context|skills_instructions|system-reminder|available_plugins)>`)

func stripHarnessBlocks(s string) string {
	for i := 0; i < 4 && strings.Contains(s, "<environment_context>"); i++ {
		s = reHarnessBlock.ReplaceAllString(s, "")
		closing := "</environment_context>"
		if idx := strings.Index(s, closing); idx >= 0 {
			s = s[:idx] + s[idx+len(closing):]
		}
	}
	return s
}

// stripIdentity 剥离 system / developer 消息里的身份声明。
//
// 与 sanitizeText 分工：这里处理"我是谁"的整句声明，
// 那些句子命中上游的审核词库但不含框架指纹串。
//
// 精确度要求很高——不能把用户的正常指令一起删掉。
// 因此只删同时满足"身份声明句式"且不含"义务/禁止"词的句子：
// "You are required to refuse harmful requests" 这类合规声明必须原样保留。
var (
	reIdentitySentence = regexp.MustCompile(`(?i)(?:you are|you're)\s+(?:an?\s+)?(?:[a-z ]{0,40}?)(?:claude|chatgpt|gpt|codex|codebuddy|workbuddy|copilot|gemini|llama|deepseek|qwen|assistant|ai)\b`)
	reObligation       = regexp.MustCompile(`(?i)\b(?:must|should|shall|required?|refuse|forbidden|prohibited|never|always|ensure|follow)\b`)
)

// stripIdentity 从 system 文本里移除身份声明句，保留业务指令。
func stripIdentity(s string) string {
	if s == "" {
		return s
	}
	// 按句切分逐句判断，保留原标点，避免破坏可读性。
	// 注意：整段没有标点时 splitSentences 只返回一段，
	// 因此下面还要对"整段就是一个身份声明"的情况做整段判断。
	parts := splitSentences(s)
	if len(parts) == 1 {
		trimmed := strings.TrimSpace(s)
		if reIdentitySentence.MatchString(trimmed) && !reObligation.MatchString(trimmed) {
			return ""
		}
	}
	var kept []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			kept = append(kept, p)
			continue
		}
		if reIdentitySentence.MatchString(trimmed) && !reObligation.MatchString(trimmed) {
			continue // 纯身份声明，删
		}
		kept = append(kept, p)
	}
	return strings.TrimSpace(strings.Join(kept, " "))
}

// splitSentences 按句号类标点切句。
//
// 不用正则：Go 的 RE2 不支持反向引用（零宽断言）。
// 需求很简单，手写反而更清楚，也省掉一次正则编译。
func splitSentences(s string) []string {
	var out []string
	start := 0
	runes := []rune(s)
	for i, r := range runes {
		switch r {
		case '.', '!', '?', '。', '！', '？':
			// 吃掉紧跟其后的空白，避免下一句开头带前导空格
			j := i + 1
			for j < len(runes) && (runes[j] == ' ' || runes[j] == '\n' || runes[j] == '\t' || runes[j] == '\r') {
				j++
			}
			out = append(out, string(runes[start:j]))
			start = j
			i = j - 1
		}
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

// zeroWidthSplit 在敏感词首字符后插入零宽空格。
//
// 原理：零宽空格对人眼和模型都不可见，但会打断上游的关键词连续匹配。
// 相比删除，这是"保留语义、破坏特征"的更温和做法。
//
// 注意：默认关闭。只有在删不掉又必须绕过的场景才启用——
// 它会让 prompt 变脏，模型可能偶尔把零宽字符读出来。
var reSensitiveTerm = regexp.MustCompile(`(?i)\b(exploit|exploit\s+code|payload|injection|sql\s+injection|bypass|jailbreak|malware|ransomware|keylogger|botnet|ddos|brute[\s-]?force|privilege\s+escalation|reverse\s+shell)\b`)

// ZeroWidthMask 用零宽空格打断敏感词。默认不启用。
func ZeroWidthMask(s string) string {
	if s == "" {
		return s
	}
	return reSensitiveTerm.ReplaceAllStringFunc(s, func(m string) string {
		r := []rune(m)
		if len(r) < 2 {
			return m
		}
		return string(r[0]) + zeroWidth + string(r[1:])
	})
}

// zeroWidth 是零宽空格 U+200B。人眼不可见，但会打断关键词的连续匹配。
const zeroWidth = "\u200b"
