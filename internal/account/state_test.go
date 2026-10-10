package account

import (
	"github.com/wb2go/wb2go/internal/config"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 回归：Save 必须自行初始化 State.Binds。
// 旧版本首启后一旦产生粘性绑定，persistLoop 调 Save 就会
// "assignment to entry in nil map" panic，网关直接 500。
func TestSaveWithStickyBindingNoPanic(t *testing.T) {
	cfg := &config.Config{}
	p := NewPool(cfg)
	p.Upsert("uid-1", "测试号", "cn", "workbuddy")

	s := NewStickyRouter(true, time.Hour, false)
	s.Bind("conv-abc", "uid-1", time.Now())

	path := filepath.Join(t.TempDir(), "state.json")
	if err := p.Save(path, s, time.Hour); err != nil {
		t.Fatalf("Save 不应报错: %v", err)
	}

	st, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState 失败: %v", err)
	}
	if st.Binds["conv-abc"] != "uid-1" {
		t.Fatalf("绑定未落盘: %+v", st.Binds)
	}
	if _, ok := st.Entries["uid-1"]; !ok {
		t.Fatalf("账号条目未落盘")
	}
}

// 回归：状态文件显式 "binds": null 时 LoadState 不应返回 nil map。
func TestLoadStateNullBinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{},"binds":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState 失败: %v", err)
	}
	if st.Binds == nil {
		t.Fatal("Binds 不应为 nil")
	}
}
