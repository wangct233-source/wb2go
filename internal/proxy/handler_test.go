package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wb2go/wb2go/internal/upstream"
)

// 回归测试：aggregate 必须把 SSE 末帧的 usage 带回 attemptResult。
//
// 曾发生的事故：attemptResult.usage 从未被赋值（恒为 nil），而
// dispatchChat 的成功分支直接解引用 res.usage.PromptTokens ——
// 每一个成功的非流式聊天请求都 nil panic。上线两天后第一个真实
// 用户（23 个账号）一发请求就触发。本测试保证 usage 永远被带回。
func TestAggregateCarriesUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"你好"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"，世界"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46,"cached_tokens":8,"reasoning_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(sse)),
	}

	res := (&Handler{}).aggregate(nil, resp, "glm-5.2")

	if res.err != nil {
		t.Fatalf("不应有错误: %v", res.err)
	}
	if res.usage == nil {
		// 正是线上炸掉的那一行：res.usage.PromptTokens
		t.Fatal("usage 为 nil —— dispatchChat 成功分支将 nil panic（回归！）")
	}
	if res.usage.PromptTokens != 12 || res.usage.CompletionTokens != 34 || res.usage.TotalTokens != 46 {
		t.Errorf("usage 数值不对: %+v", res.usage)
	}
	if res.usage.CachedTokens != 8 || res.usage.ReasoningTokens != 5 {
		t.Errorf("扩展字段丢失: %+v", res.usage)
	}
}

// 无 usage 帧（上游异常截断）时也不得 panic —— dispatchChat 有判空，
// 但这里验证 nil 是合法返回值而非错误路径。
func TestAggregateNilUsageTolerated(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	res := (&Handler{}).aggregate(nil, resp, "glm-5.2")
	if res.err != nil || res.write == nil {
		t.Fatalf("成功路径应返回 write 闭包: err=%v", res.err)
	}
	if res.usage != nil {
		t.Logf("上游没发 usage 帧，nil 属预期")
	}
}

// Aggregator.Usage() getter 与帧内 usage 提取的一致性。
func TestAggregatorUsageGetter(t *testing.T) {
	agg := upstream.NewAggregator()
	if agg.Usage() != nil {
		t.Fatal("空聚合器 Usage 应为 nil")
	}
	agg.Add(&upstream.ChatChunk{Usage: &upstream.Usage{TotalTokens: 9}})
	if agg.Usage() == nil || agg.Usage().TotalTokens != 9 {
		t.Fatal("Usage getter 未取到帧内用量")
	}
}
