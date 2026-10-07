package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wb2go/wb2go/internal/config"
	"github.com/wb2go/wb2go/internal/store"
	"github.com/wb2go/wb2go/internal/upstream"
)

// startOAuth 发起设备授权，返回浏览器要打开的授权链接。
//
// 流程与官方 CLI 一致（无 PKCE，state 由服务端签发）：
//  1. POST /plugin/auth/state?platform=CLI  → 拿 state + authUrl
//  2. 用户在浏览器完成登录
//  3. 轮询 GET /plugin/auth/token?state=...  → 拿 token
func (s *Server) startOAuth(ctx context.Context, realm string) (string, error) {
	cfg := s.d.Cfg.Snapshot()
	id, err := upstream.BuildIdentity(cfg.Identity, cfg.DesktopVer, cfg.CLIVersion, cfg.ProductLabel)
	if err != nil {
		return "", err
	}
	id = id.Resolve(realm)

	headers := map[string]string{
		"User-Agent":       id.UserAgent,
		"X-Product":        id.Product,
		"X-IDE-Name":       id.IdeName,
		"X-IDE-Type":       id.IdeType,
		"X-IDE-Version":    id.DesktopVer,
		"Accept":           "application/json",
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           id.BillingBase,
		"Referer":          id.BillingBase + "/",
		"Accept-Language":  "zh-CN",
	}
	url := id.ChatBase + upstream.PathAuthState + "?platform=CLI"
	resp, err := s.d.API.Client.PostJSON(ctx, url, headers, map[string]any{})
	if err != nil {
		return "", err
	}
	raw, err := upstream.ReadCST(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("上游响应不是合法 JSON: %w", err)
	}
	state := stringOf(body["state"])
	authURL := stringOf(body["authUrl"])
	if authURL == "" {
		authURL = stringOf(body["auth_url"])
	}
	if state == "" || authURL == "" {
		return "", fmt.Errorf("上游未返回授权链接（可能官方已调整该接口）")
	}

	s.oauthMu.Lock()
	s.oauthState = state
	s.oauthRealm = realm
	s.oauthStart = time.Now()
	s.oauthSeen = false
	s.oauthMu.Unlock()
	return authURL, nil
}

// pollOAuth 轮询授权结果。返回 (uid, 完成)。
func (s *Server) pollOAuth(ctx context.Context, state string) (string, bool) {
	cfg := s.d.Cfg.Snapshot()
	s.oauthMu.Lock()
	realm := s.oauthRealm
	s.oauthMu.Unlock()
	if realm == "" {
		realm = "cn"
	}
	id, err := upstream.BuildIdentity(cfg.Identity, cfg.DesktopVer, cfg.CLIVersion, cfg.ProductLabel)
	if err != nil {
		return "", false
	}
	id = id.Resolve(realm)

	headers := map[string]string{
		"User-Agent":       id.UserAgent,
		"X-Product":        id.Product,
		"X-Request-ID":     "oauth-poll",
		"Accept":           "application/json",
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           id.BillingBase,
		"Referer":          id.BillingBase + "/",
	}
	// token 轮询
	url := id.ChatBase + upstream.PathAuthToken + "?state=" + state
	resp, err := s.d.API.Client.GetJSON(ctx, url, headers)
	if err != nil {
		return "", false
	}
	raw, err := upstream.ReadCST(resp)
	if err != nil {
		return "", false
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return "", false
	}
	// 11217 = 等待用户完成授权
	if code := stringOf(body["code"]); code == "11217" || code == "11218" || code == "1001" {
		return "", false
	}
	access := stringOf(body["accessToken"])
	if access == "" {
		access = stringOf(body["access_token"])
	}
	if access == "" {
		return "", false
	}
	refresh := stringOf(body["refreshToken"])
	if refresh == "" {
		refresh = stringOf(body["refresh_token"])
	}

	// 取账号信息（拿到 uid 与昵称）
	uid, nickname := s.fetchAccountInfo(ctx, id, access)
	if uid == "" {
		return "", false
	}
	creds := store.Credentials{
		UID: uid, Nickname: nickname, Realm: realm,
		Identity: cfg.Identity, AccessToken: access, RefreshToken: refresh,
		ExpiresAt: time.Now().Unix() + 3600, Source: "oauth",
	}
	if err := s.d.Store.Save(creds); err != nil {
		return "", false
	}
	return uid, true
}

// oauthRealmSnapshot 读出当前授权会话的区域（供 status 接口返回）。
func (s *Server) oauthRealmSnapshot() string {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauthRealm == "" {
		return s.d.Cfg.Snapshot().DefaultRealm
	}
	return s.oauthRealm
}

// fetchAccountInfo 拉取账号昵称。
// 拿不到昵称不影响登录 —— uid 已经足够把账号放进池里。
func (s *Server) fetchAccountInfo(ctx context.Context, id upstream.Identity, access string) (uid, nickname string) {
	headers := map[string]string{
		"Authorization":    "Bearer " + access,
		"User-Agent":       id.UserAgent,
		"X-Product":        id.Product,
		"Accept":           "application/json",
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           id.BillingBase,
		"Referer":          id.BillingBase + "/",
	}
	resp, err := s.d.API.Client.GetJSON(ctx, id.ChatBase+upstream.PathLoginAccount, headers)
	if err != nil {
		return "", ""
	}
	raw, err := upstream.ReadCST(resp)
	if err != nil {
		return "", ""
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return "", ""
	}
	acct, _ := body["account"].(map[string]any)
	if acct == nil {
		acct = body
	}
	uid = stringOf(acct["uid"])
	if uid == "" {
		uid = stringOf(acct["userId"])
	}
	nickname = stringOf(acct["nickname"])
	if nickname == "" {
		nickname = stringOf(acct["nickName"])
	}
	return uid, nickname
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// warmupRealm 在启动时预热两个区域的可达性（只打日志，不阻断启动）。
func warmupRealm(ctx context.Context, api *upstream.API, cfg *config.Config) {
	for _, realm := range []string{"cn", "intl"} {
		id, err := upstream.BuildIdentity(cfg.Identity, cfg.DesktopVer, cfg.CLIVersion, cfg.ProductLabel)
		if err != nil {
			return
		}
		id = id.Resolve(realm)
		_ = id
	}
}
