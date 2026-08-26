package build

import (
	"strings"
	"testing"
	"time"
)

func TestNotifyTemplateUsesCommitMeta(t *testing.T) {
	cases := []struct {
		name  string
		title string
		body  string
		want  bool
	}{
		{"标题含作者", "由 {{commitAuthor}} 提交", "", true},
		{"正文含备注", "", "提交信息:{{commitMessage}}", true},
		{"正文含时间", "通知", "时间 {{commitTime}}", true},
		{"大小写不敏感", "{{COMMITMESSAGE}}", "", true},
		{"无提交占位", "{{project}} 构建完成", "分支 {{branch}}", false},
		{"空模板", "", "", false},
	}
	for _, c := range cases {
		if got := notifyTemplateUsesCommitMeta(c.title, c.body); got != c.want {
			t.Errorf("%s: notifyTemplateUsesCommitMeta(%q,%q) = %v, want %v",
				c.name, c.title, c.body, got, c.want)
		}
	}
}

func TestHasPlaceholder(t *testing.T) {
	if !hasPlaceholder("部署 {{commitAuthor}} 完成", "commitAuthor") {
		t.Error("exact placeholder should match")
	}
	if !hasPlaceholder("部署 {{COMMITMESSAGE}} 完成", "commitMessage") {
		t.Error("case-insensitive placeholder should match")
	}
	if hasPlaceholder("部署 {{commit}} 完成", "commitAuthor") {
		t.Error("different placeholder should not match")
	}
	if hasPlaceholder("无占位文本", "commitTime") {
		t.Error("no placeholder should not match")
	}
}

func TestCommitMetaLogLine(t *testing.T) {
	resolved := &CloneResolved{
		CommitShort: "abcdef0",
		Author:      "Alice",
		Message:     "fix: bug",
		Time:        time.Date(2026, 8, 26, 10, 0, 0, 0, time.Local),
	}

	// 完整元数据
	got := commitMetaLogLine("https://git.example.com/pipewright.git", "abcdef0", resolved)
	for _, want := range []string{"https://git.example.com/pipewright.git", "abcdef0",
		"作者 Alice", "提交备注 fix: bug", "提交时间"} {
		if !strings.Contains(got, want) {
			t.Errorf("commitMetaLogLine missing %q in %q", want, got)
		}
	}

	// nil resolved → 无元数据后缀
	gotNil := commitMetaLogLine("https://git.example.com/pipewright.git", "abcdef0", nil)
	if strings.Contains(gotNil, "作者") {
		t.Errorf("nil resolved should omit metadata, got: %q", gotNil)
	}

	// 空 commitTag → latest 兜底
	gotLatest := commitMetaLogLine("https://git.example.com/pipewright.git", "", resolved)
	if !strings.Contains(gotLatest, "@ latest") {
		t.Errorf("empty commitTag should fall back to latest, got: %q", gotLatest)
	}

	// 空字段省略(不输出空段)
	sparse := commitMetaLogLine("u", "sha", &CloneResolved{CommitShort: "sha"})
	if strings.Contains(sparse, "作者") || strings.Contains(sparse, "[") {
		t.Errorf("empty fields should be omitted, got: %q", sparse)
	}
}
