package build

import (
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
)

func TestCommitMetaAsEnv(t *testing.T) {
	resolved := &CloneResolved{
		CommitShort: "abcdef0",
		Author:      "Alice",
		Message:     "fix: bug",
		Time:        time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC),
	}
	env := commitMetaAsEnv(resolved)
	want := map[string]string{
		"COMMIT_SHA":    "abcdef0",
		"COMMIT_AUTHOR": "Alice",
		"COMMIT_MESSAGE": "fix: bug",
		"COMMIT_TIME":   "2026-08-26T10:00:00Z",
	}
	if len(env) != len(want) {
		t.Fatalf("len(env) = %d, want %d (%+v)", len(env), len(want), env)
	}
	for _, v := range env {
		if w, ok := want[v.Key]; !ok {
			t.Errorf("unexpected key %q", v.Key)
		} else if v.Value != w {
			t.Errorf("%s = %q, want %q", v.Key, v.Value, w)
		}
		if v.Secret {
			t.Errorf("%s Secret should be false", v.Key)
		}
	}

	// 空 resolved → 空切片
	if got := commitMetaAsEnv(nil); got != nil {
		t.Errorf("nil resolved → %+v, want nil", got)
	}
	if got := commitMetaAsEnv(&CloneResolved{}); len(got) != 0 {
		t.Errorf("empty resolved → %+v, want empty", got)
	}
}

func TestExpandTagPlaceholders(t *testing.T) {
	r := &run.Run{ID: "42", Trigger: run.Trigger{Branch: "main"}}
	resolved := &CloneResolved{
		CommitShort: "abcdef0",
		Author:      "Alice",
		Message:     "fix: bug",
		Time:        time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC),
	}

	cases := []struct {
		tag  string
		want string
	}{
		{"myapp:${COMMIT_SHA}", "myapp:abcdef0"},
		{"myapp:${BRANCH}", "myapp:main"},
		{"myapp:${BUILD_NUMBER}", "myapp:42"},
		{"myapp:${COMMIT_AUTHOR}", "myapp:Alice"},
		{"myapp:${COMMIT_MESSAGE}", "myapp:fix: bug"},
		{"myapp:${COMMIT_TIME}", "myapp:2026-08-26T10:00:00Z"},
		{"myapp:latest", "myapp:latest"},                                  // 无占位符,零开销直返
		{"myapp:${UNKNOWN}", "myapp:${UNKNOWN}"},                          // 未识别占位符保留
	}
	for _, c := range cases {
		var rr *run.Run
		if c.tag == "myapp:${BRANCH}" || c.tag == "myapp:${BUILD_NUMBER}" {
			rr = r // 仅这两个 case 提供 r
		}
		got := expandTagPlaceholders(c.tag, rr, resolved)
		if got != c.want {
			t.Errorf("expandTagPlaceholders(%q) = %q, want %q", c.tag, got, c.want)
		}
	}
}

func TestComposeTemplateContext(t *testing.T) {
	resolved := &CloneResolved{CommitShort: "abcdef0", Author: "Alice", Message: "fix: bug", Time: time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)}
	cfg := map[string]any{"image": "node:20", "commands": "npm ci"}
	ctx := composeTemplateContext(cfg, resolved)
	for k, want := range map[string]string{
		"commit_sha":   "abcdef0",
		"commit_author": "Alice",
		"commit_message": "fix: bug",
		"commit_time":  "2026-08-26T10:00:00Z",
	} {
		if ctx[k] != want {
			t.Errorf("ctx[%q] = %q, want %q", k, ctx[k], want)
		}
	}
	// nil resolved → 仅 config 字段
	ctxNil := composeTemplateContext(cfg, nil)
	if _, ok := ctxNil["commit_sha"]; ok {
		t.Error("nil resolved should not add commit_sha")
	}
	if ctxNil["image"] != "node:20" {
		t.Errorf("config field lost: %q", ctxNil["image"])
	}
}
