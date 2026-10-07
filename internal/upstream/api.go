package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// 短 RPC 端点路径（相对 BillingBase）。
//
// 全部是逆向得到的非公开接口，上游没有公开文档。
// 把它们集中成一组常量，是为了让"改上游"这件事只需要改这一个文件。
const (
	PathChatCompletions = "/v2/chat/completions"
	PathModels          = "/v2/enterprises/personal/models"
	PathProductConfig   = "/v3/config"

	PathTokenRefresh = "/v2/plugin/auth/token/refresh"
	PathAuthState    = "/v2/plugin/auth/state"
	PathAuthToken    = "/v2/plugin/auth/token"
	PathLoginAccount = "/v2/plugin/login/account"

	PathCheckin      = "/v2/billing/meter/daily-checkin"
	PathCheckinStat  = "/v2/billing/meter/checkin-status"
	PathUserResource = "/v2/billing/meter/get-user-resource"

	PathReport = "/v2/report"

	PathGrowthTasks  = "/v2/activity/growth/tasks"
	PathGrowthAccept = "/v2/activity/growth/tasks/accept"
	PathGrowthStreak = "/v2/activity/growth/streak"
	PathGrowthRedeem = "/v2/activity/growth/redeem"
	PathLotterySum   = "/v2/activity/growth/lottery/summary"
	PathLotteryDraw  = "/v2/activity/growth/lottery/draw"
	PathBuddyAgree   = "/v2/activity/growth/buddy/agreement"
	PathBuddyFirst   = "/v2/activity/growth/buddy/first"
	PathBuddyInfo    = "/v2/activity/growth/buddy/info"
	PathTravelStatus = "/v2/activity/growth/buddy/travel/status"
	PathTravelDepart = "/v2/activity/growth/buddy/travel/depart"
	PathTravelClaim  = "/v2/activity/growth/buddy/travel/claim"
)

// Creds 是一次 RPC 需要的账号信息。
type Creds struct {
	UID          string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Realm        string
	Identity     string
	EnterpriseID string
	Nickname     string
}

// API 是上游短 RPC 的调用面。
type API struct {
	Client   *Client
	Identity func(realm, identity string) Identity
	FP       func(uid string, id Identity) Fingerprint
}

// ChatHeaders 构造聊天请求的头族。
//
// X-Refresh-Token 永不出现——它只属于 token 刷新请求。
// 聊天请求带上它会让上游认为凭证被篡改。
func (a *API) ChatHeaders(c Creds, id Identity, seq int64) map[string]string {
	fp := a.FP(c.UID, id)
	h := map[string]string{
		"Authorization":   "Bearer " + c.AccessToken,
		"User-Agent":      id.UserAgent,
		"Accept":          "text/event-stream",
		"X-Product":       id.Product,
		"X-IDE-Name":      id.IdeName,
		"X-IDE-Type":      id.IdeType,
		"X-IDE-Version":   id.DesktopVer,
		"X-Request-ID":    stableRequestID(fp.MachineID, seq),
		"X-Machine-ID":    fp.MachineID,
		"X-Session-ID":    fp.SessionID,
		"X-Device-Token":  fp.MachineID,
		"Accept-Language": "zh-CN",
	}
	if c.UID != "" {
		h["X-User-Id"] = c.UID
	} else {
		// 缺失时发显式的 No-* 头，而不是省略 ——
		// 官方客户端就是显式发这些头，省略反而是特征。
		h["X-No-User-Id"] = "1"
	}
	if c.EnterpriseID != "" {
		h["X-Enterprise-Id"] = c.EnterpriseID
		h["X-Tenant-Id"] = c.EnterpriseID
	} else {
		h["X-No-Enterprise-Id"] = "1"
	}
	h["X-No-Department-Info"] = "1"
	return h
}

// BillingHeaders 构造签到 / 积分类请求的头族（形态与聊天不同）。
func (a *API) BillingHeaders(c Creds, id Identity) map[string]string {
	fp := a.FP(c.UID, id)
	lang := "zh-CN"
	if c.Realm == "intl" {
		lang = "en-US"
	}
	h := map[string]string{
		"Authorization":       "Bearer " + c.AccessToken,
		"User-Agent":          id.UserAgent,
		"X-Requested-With":    "XMLHttpRequest",
		"Accept-Language":     lang,
		"X-CodeBuddy-Request": "1",
		"X-Machine-ID":        fp.MachineID,
		"X-Session-ID":        fp.SessionID,
		"X-Request-ID":        fp.RequestID,
		"Origin":              id.BillingBase,
		"Referer":             id.BillingBase + "/",
	}
	if c.UID != "" {
		h["X-User-Id"] = c.UID
	}
	if c.EnterpriseID != "" {
		h["X-Enterprise-Id"] = c.EnterpriseID
		h["X-Tenant-Id"] = c.EnterpriseID
	} else {
		h["X-No-Enterprise-Id"] = "1"
	}
	return h
}

// RefreshHeaders 构造 token 刷新请求头。
func (a *API) RefreshHeaders(c Creds, id Identity) map[string]string {
	h := a.BillingHeaders(c, id)
	h["X-Refresh-Token"] = c.RefreshToken
	source := "plugin"
	if c.Realm == "cn" {
		source = "workbuddy"
	}
	h["X-Auth-Refresh-Source"] = source
	return h
}

// RPCResult 是一次短 RPC 的结果。
type RPCResult struct {
	OK       bool
	Status   int
	Body     map[string]any
	Raw      string
	APIError *APIError
}

// Code 取业务 code。
func (r *RPCResult) Code() string {
	if r.Body == nil {
		return ""
	}
	return toStr(r.Body["code"])
}

// Message 取业务 message。
func (r *RPCResult) Message() string {
	if r.Body == nil {
		return ""
	}
	if m, ok := r.Body["msg"].(string); ok {
		return m
	}
	if m, ok := r.Body["message"].(string); ok {
		return m
	}
	return ""
}

// do 发起一次 RPC 并解析响应。
func (a *API) do(ctx context.Context, method, base, path string, headers map[string]string, body any) *RPCResult {
	url := base + path
	var (
		resp *http.Response
		err  error
	)
	if method == http.MethodGet {
		resp, err = a.Client.GetJSON(ctx, url, headers)
	} else {
		resp, err = a.Client.PostJSON(ctx, url, headers, body)
	}
	if err != nil {
		return &RPCResult{OK: false, APIError: &APIError{Kind: KindTransport, Msg: err.Error()}}
	}
	raw, err := ReadCST(resp)
	if err != nil {
		return &RPCResult{OK: false, APIError: &APIError{Kind: KindTransport, Msg: "读取响应失败: " + err.Error()}}
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed) // 非 JSON 也不致命，Raw 里还有

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &RPCResult{
			OK: false, Status: resp.StatusCode, Body: parsed, Raw: string(raw),
			APIError: Classify(resp.StatusCode, raw, ""),
		}
	}
	// HTTP 200 但业务 code 非 0 也算失败（上游常见做法）
	if code := toStr(parsed["code"]); code != "" && code != "0" && code != "200" {
		return &RPCResult{
			OK: false, Status: resp.StatusCode, Body: parsed, Raw: string(raw),
			APIError: Classify(400, raw, ""),
		}
	}
	return &RPCResult{OK: true, Status: resp.StatusCode, Body: parsed, Raw: string(raw)}
}

// RefreshToken 刷新 token。成功后调用方需持久化新 token。
func (a *API) RefreshToken(ctx context.Context, c Creds) (string, string, int64, *RPCResult) {
	id := a.Identity(c.Realm, c.Identity)
	res := a.do(ctx, http.MethodPost, id.BillingBase, PathTokenRefresh, a.RefreshHeaders(c, id), nil)
	if !res.OK {
		return c.AccessToken, c.RefreshToken, c.ExpiresAt, res
	}
	newAccess, _ := res.Body["accessToken"].(string)
	if v, ok := res.Body["access_token"].(string); ok && v != "" {
		newAccess = v
	}
	newRefresh := c.RefreshToken
	if v, ok := res.Body["refreshToken"].(string); ok && v != "" {
		newRefresh = v
	}
	if v, ok := res.Body["refresh_token"].(string); ok && v != "" {
		newRefresh = v
	}
	var expires int64
	for _, k := range []string{"expiresIn", "expires_in"} {
		if v, ok := res.Body[k].(float64); ok {
			expires = nowMillis()/1000 + int64(v)
			break
		}
	}
	if newAccess == "" {
		newAccess = c.AccessToken
	}
	return newAccess, newRefresh, expires, res
}

// FetchCredits 查询余额与积分包。
type CreditsInfo struct {
	Remaining    float64
	Unlimited    bool
	ExpireAt     int64
	PackageCount int
}

// FetchCredits 拉取账号余额。
func (a *API) FetchCredits(ctx context.Context, c Creds) (*CreditsInfo, *RPCResult) {
	id := a.Identity(c.Realm, c.Identity)
	res := a.do(ctx, http.MethodPost, id.BillingBase, PathUserResource, a.BillingHeaders(c, id), map[string]any{})
	if !res.OK {
		return nil, res
	}
	info := &CreditsInfo{}
	// 响应结构在不同版本间有差异，这里做宽松解析：
	// 先找包列表，找不到再看顶层字段。
	if data, ok := res.Body["data"].(map[string]any); ok {
		if pkg, ok := data["packages"].([]any); ok {
			info.PackageCount = len(pkg)
			var earliest int64
			for _, p := range pkg {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				// 优先用精确字段；CapacityRemainPrecise 带小数，是权威值
				remain := numOf(pm["CycleCapacityRemainPrecise"])
				if remain == 0 {
					remain = numOf(pm["remain"])
				}
				if remain == 0 {
					remain = numOf(pm["remaining"])
				}
				cap := numOf(pm["CycleCapacity"])
				if cap == 0 {
					cap = numOf(pm["capacity"])
				}
				// 防御脏数据：remain 不该超过 capacity
				if cap > 0 && remain > cap {
					remain = cap
				}
				info.Remaining += remain
				if exp := int64(numOf(pm["expireTime"])); exp > 0 && (earliest == 0 || exp < earliest) {
					earliest = exp
				}
			}
			info.ExpireAt = earliest
			return info, res
		}
		info.Remaining = numOf(data["remain"])
		info.Unlimited = boolOf(data["unlimited"])
	}
	if info.Remaining == 0 {
		info.Remaining = numOf(res.Body["remain"])
	}
	return info, res
}

func numOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		var f float64
		_, _ = fmt.Sscanf(t, "%g", &f)
		return f
	}
	return 0
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// Checkin 每日签到。
func (a *API) Checkin(ctx context.Context, c Creds) *RPCResult {
	id := a.Identity(c.Realm, c.Identity)
	headers := a.BillingHeaders(c, id)
	// 先查状态：今天已签到就不重复发，重复签到会被上游记为异常
	if st := a.do(ctx, http.MethodGet, id.BillingBase, PathCheckinStat, headers, nil); st.OK {
		if alreadyCheckedIn(st.Body) {
			return &RPCResult{OK: true, Status: 200, Body: map[string]any{"skipped": true, "reason": "今日已签到"}}
		}
	}
	return a.do(ctx, http.MethodPost, id.BillingBase, PathCheckin, headers, map[string]any{})
}

// alreadyCheckedIn 判断是否已签到。
// 上游用多种业务码表示"已领取"，不同版本码值不一致。
func alreadyCheckedIn(body map[string]any) bool {
	code := toStr(body["code"])
	switch code {
	case "10001", "14001", "1001":
		return true
	}
	for _, k := range []string{"checkedIn", "checked_in", "isCheckedIn", "signed"} {
		if boolOf(body[k]) {
			return true
		}
	}
	return false
}

// ReportEvents 上报行为事件（点亮成长任务用）。
func (a *API) ReportEvents(ctx context.Context, c Creds, events []any) *RPCResult {
	id := a.Identity(c.Realm, c.Identity)
	return a.do(ctx, http.MethodPost, id.BillingBase, PathReport, a.BillingHeaders(c, id), events)
}

// FetchTasks 拉取成长任务列表。
func (a *API) FetchTasks(ctx context.Context, c Creds) ([]map[string]any, *RPCResult) {
	id := a.Identity(c.Realm, c.Identity)
	res := a.do(ctx, http.MethodGet, id.BillingBase, PathGrowthTasks, a.BillingHeaders(c, id), nil)
	if !res.OK {
		return nil, res
	}
	var arr []map[string]any
	if data, ok := res.Body["data"].(map[string]any); ok {
		if list, ok := data["tasks"].([]any); ok {
			for _, t := range list {
				if tm, ok := t.(map[string]any); ok {
					arr = append(arr, tm)
				}
			}
		}
	}
	if arr == nil {
		if list, ok := res.Body["tasks"].([]any); ok {
			for _, t := range list {
				if tm, ok := t.(map[string]any); ok {
					arr = append(arr, tm)
				}
			}
		}
	}
	return arr, res
}

// AcceptTasks 批量接取任务。
func (a *API) AcceptTasks(ctx context.Context, c Creds, codes []string) *RPCResult {
	id := a.Identity(c.Realm, c.Identity)
	return a.do(ctx, http.MethodPost, id.BillingBase, PathGrowthAccept, a.BillingHeaders(c, id),
		map[string]any{"task_codes": codes})
}

// ClaimTask 领取任务奖励。
//
// 注意：领奖必须走 Web 域（www.workbuddy.cn），不是 CLI 域。
// 走错域会返回 200 但不记账——这是领奖失败最常见的原因。
func (a *API) ClaimTask(ctx context.Context, c Creds, code string) *RPCResult {
	id := a.Identity(c.Realm, c.Identity)
	headers := a.BillingHeaders(c, id)
	headers["x-client-platform"] = "web"
	headers["Origin"] = CNWebBase
	headers["Referer"] = CNWebBase + "/"
	res := a.do(ctx, http.MethodPost, CNWebBase, PathGrowthTasks+"/"+code+"/claim", headers, nil)
	return res
}

// FetchModels 拉取模型目录。
func (a *API) FetchModels(ctx context.Context, c Creds) ([]any, *RPCResult) {
	id := a.Identity(c.Realm, c.Identity)
	res := a.do(ctx, http.MethodGet, id.ChatBase, PathModels, a.ChatHeaders(c, id, 0), nil)
	if !res.OK {
		return nil, res
	}
	var arr []any
	if data, ok := res.Body["data"].([]any); ok {
		arr = data
	} else if data, ok := res.Body["models"].([]any); ok {
		arr = data
	}
	return arr, res
}

// BuddyTravel 推进猫猫旅行状态机一步。
//
// 只做当前状态允许的那一个动作：在家就派出、到了就领奖、在路上就跳过。
// 轮询等待没有意义——上游本来就规定一天只能派一次。
func (a *API) BuddyTravel(ctx context.Context, c Creds) (string, *RPCResult) {
	if c.Realm != "cn" {
		return "", nil // 旅行仅国内版
	}
	id := a.Identity(c.Realm, c.Identity)
	headers := a.BillingHeaders(c, id)

	info := a.do(ctx, http.MethodGet, id.BillingBase, PathBuddyInfo, headers, nil)
	if !info.OK {
		return "", info
	}
	data, _ := info.Body["data"].(map[string]any)
	buddy, _ := data["buddy"].(map[string]any)
	if buddy == nil {
		// 无猫：先同意协议（幂等）再尝试领养
		a.do(ctx, http.MethodPost, id.BillingBase, PathBuddyAgree, headers, map[string]any{})
		res := a.do(ctx, http.MethodPost, id.BillingBase, PathBuddyFirst, headers, map[string]any{})
		if res.OK {
			return "已领养 Buddy", res
		}
		return "领养未达成: " + res.Message(), res
	}

	// 有猫：查旅行状态并推进
	st := a.do(ctx, http.MethodGet, id.BillingBase, PathTravelStatus, headers, nil)
	if !st.OK {
		return "", st
	}
	sd, _ := st.Body["data"].(map[string]any)
	state, _ := sd["state"].(string)
	switch state {
	case "arrived":
		res := a.do(ctx, http.MethodPost, id.BillingBase, PathTravelClaim, headers,
			map[string]any{"record_id": sd["record_id"]})
		if res.OK {
			return "已领取到站奖励", res
		}
		return "领奖失败: " + res.Message(), res
	case "idle":
		res := a.do(ctx, http.MethodPost, id.BillingBase, PathTravelDepart, headers,
			map[string]any{"location_id": 4})
		if res.OK {
			return "已派出旅行", res
		}
		return "派出失败: " + res.Message(), res
	case "traveling":
		return "在路上（跳过）", nil
	default:
		return "状态未知: " + state, nil
	}
}

// JSONBody 构造请求体（供外部复用）。
func JSONBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// TrimPrefixSlash 去掉路径前导斜杠（拼接 base 时用）。
func TrimPrefixSlash(p string) string { return strings.TrimPrefix(p, "/") }
