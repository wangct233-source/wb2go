package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidUIDBlocksPathTraversal 是路径穿越防线。
//
// uid 会直接参与拼出凭证文件路径。若不校验，形如 "../../.ssh/authorized_keys"
// 的 uid 就能写到 accounts 目录之外 —— 这是真实可利用的漏洞，
// 因此白名单必须是硬约束，不能靠调用方自觉。
func TestValidUIDBlocksPathTraversal(t *testing.T) {
	bad := []string{
		"../../../etc/passwd",
		"..\\..\\windows\\system32",
		"a/b",
		"a\\b",
		"has space",
		"has.dot",
		"",
		"中文uid",
		"semi;colon",
		"pipe|it",
	}
	for _, uid := range bad {
		if ValidUID(uid) {
			t.Errorf("uid %q 含路径穿越或非法字符，应被拒绝", uid)
		}
	}
	good := []string{"0851ce35a", "abc-123_456", "A", "0123456789abcdef0123456789abcdef"}
	for _, uid := range good {
		if !ValidUID(uid) {
			t.Errorf("uid %q 是合法格式，不应被拒绝", uid)
		}
	}
	// 超长也应拒绝（防止超长文件名）
	if ValidUID(string(make([]byte, 65))) {
		t.Error("超过 64 字符的 uid 应被拒绝")
	}
}

// TestSaveRejectsTraversal 验证 Save 在落盘前就拦住恶意 uid。
func TestSaveRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Save(Credentials{UID: "../../escape", AccessToken: "x"})
	if err == nil {
		t.Fatal("含路径穿越的 uid 应被拒绝写入")
	}
	// 确认目录外没有产生文件
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "escape.json")); statErr == nil {
		t.Fatal("文件写到了 accounts 目录之外")
	}
}

// TestSaveAndReloadRoundTrip 验证存取往返不丢字段。
func TestSaveAndReloadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := Credentials{
		UID: "abc123", Nickname: "测试账号", Realm: "cn", Identity: "vscode",
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1700000000,
		EnterpriseID: "e1", ProxySlot: "slot1",
	}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("abc123")
	if !ok {
		t.Fatal("保存后应能读回")
	}
	if got.Nickname != in.Nickname || got.Realm != in.Realm || got.Identity != in.Identity ||
		got.AccessToken != in.AccessToken || got.RefreshToken != in.RefreshToken ||
		got.ExpiresAt != in.ExpiresAt || got.EnterpriseID != in.EnterpriseID || got.ProxySlot != in.ProxySlot {
		t.Errorf("往返后字段不一致：%+v", got)
	}
}

// TestReloadSkipsCorruptFiles 验证损坏文件不影响其他账号加载。
//
// 单个文件读失败不该让整个网关起不来 —— 那等于一个坏文件废掉全部账号。
func TestReloadSkipsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Credentials{UID: "good1", AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	// 写一个损坏的 JSON
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Credentials{UID: "good2", AccessToken: "b"}); err != nil {
		t.Fatal(err)
	}
	// 重新加载：两个正常账号都应还在
	fresh, err := New(dir)
	if err != nil {
		t.Fatalf("损坏文件不应导致 New 失败: %v", err)
	}
	if len(fresh.List()) != 2 {
		t.Errorf("应加载 2 个有效账号，实际 %d", len(fresh.List()))
	}
}

// TestRemoveDeletesFile 验证移除会删掉磁盘文件。
func TestRemoveDeletesFile(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Credentials{UID: "del1", AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("del1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("del1"); ok {
		t.Error("移除后内存中不应再有该账号")
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-del1.json")); !os.IsNotExist(err) {
		t.Error("移除后磁盘文件应已删除")
	}
	// 移除不存在的账号不应报错
	if err := s.Remove("not-exist"); err != nil {
		t.Errorf("移除不存在的账号不应报错: %v", err)
	}
}

// TestNeedRefresh 验证刷新判定的边界。
func TestNeedRefresh(t *testing.T) {
	cases := []struct {
		name      string
		creds     Credentials
		now       int64
		need      bool
		reasoning string
	}{
		{"无 refresh token 不刷新", Credentials{ExpiresAt: 1000}, 900, false,
			"没有 refresh token，无从刷新"},
		{"过期时间未知则刷新", Credentials{RefreshToken: "r", ExpiresAt: 0}, 900, true,
			"过期时间未知，保守刷新一次"},
		{"距过期充足", Credentials{RefreshToken: "r", ExpiresAt: 2000}, 900, false,
			"还有 1100 秒，远大于 120 秒阈值"},
		{"距过期不足 120 秒", Credentials{RefreshToken: "r", ExpiresAt: 950}, 900, true,
			"距过期 50 秒 < 120 秒阈值"},
	}
	for _, tc := range cases {
		if got := tc.creds.NeedRefresh(tc.now); got != tc.need {
			t.Errorf("%s: NeedRefresh = %v，期望 %v（%s）", tc.name, got, tc.need, tc.reasoning)
		}
	}
}

// TestDisplay 不泄露完整 UID。
func TestDisplay(t *testing.T) {
	if got := (Credentials{Nickname: "我的账号"}).Display(); got != "我的账号" {
		t.Errorf("有昵称时应优先用昵称，实际 %q", got)
	}
	if got := (Credentials{UID: "0123456789abcdef"}).Display(); got != "01234567" {
		t.Errorf("无昵称时应截断 UID 到 8 位，实际 %q", got)
	}
	if got := (Credentials{UID: "short"}).Display(); got != "short" {
		t.Errorf("短 UID 应原样返回，实际 %q", got)
	}
}
