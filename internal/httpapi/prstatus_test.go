package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/gitrefresh"
	"github.com/huangchengsir/pipewright/internal/oauth"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/prstatus"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// stubProberPR 是不触网的远端探测桩(本测试只走 SQL 种子 + Get/Update,实际不会被调用)。
type stubProberPR struct{}

func (stubProberPR) Probe(_ context.Context, _ /*repoURL*/ string, _ string, _ string) (string, error) {
	return "main", nil
}

// prStatusHookFixture 起一个把所有平台 API 收编到本地的 httptest server,并装好真 run/project/vault
// 服务 + 经种子建一个 GitHub 项目(带可 Reveal 的凭据)与一个带 commit 的成功 run。
// 返回:hits(server 命中计数指针)、runID、projID、project.Service、reporter。
func prStatusHookFixture(t *testing.T) (*int32, string, string, project.Service, *prstatus.Reporter, run.Service, vault.Vault) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"state":"success"}`))
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(t.TempDir() + "/prstatus.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	v := vault.New(st.DB, testMasterKey())
	// 真凭据(git_token),Reveal 取得明文 token(否则钩子会因无凭据静默跳过)。
	cred, err := v.Create(vault.CreateInput{Name: "gh", Type: vault.TypeGitToken, Secret: "ghp_token_xyz"})
	if err != nil {
		t.Fatalf("vault.Create: %v", err)
	}

	projSvc := project.New(st.DB, v, stubProberPR{})
	// 直接 SQL 种子项目(GitHub 仓库 → Detect 命中;默认 pr_status_enabled=0)。
	projID := "proj-prstatus"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB.Exec(
		`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		projID, "demo", "https://github.com/acme/demo.git", "main", cred.ID, now, now,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	runSvc := run.New(st.DB)
	r, err := runSvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abcdef1234567890"})
	if err != nil {
		t.Fatalf("run.Create: %v", err)
	}

	reporter := prstatus.NewReporter(srv.Client()).WithBaseURLs(srv.URL, srv.URL)
	return &hits, r.ID, projID, projSvc, reporter, runSvc, v
}

// TestPRStatusHookGatedByProjectFlag 断言:flag 关 + 无全局强开 → 不回写;flag 开 → 回写。
func TestPRStatusHookGatedByProjectFlag(t *testing.T) {
	hits, runID, _, projSvc, reporter, runSvc, v := prStatusHookFixture(t)

	// 1) flag 关 + globalOverride=false → 不应命中平台 API。
	hook := NewPRStatusHook(runSvc, projSvc, v, nil, reporter, "https://pw.example.com", false)
	hook(context.Background(), runID, run.StatusSuccess)
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("flag 关 + 无全局强开应跳过回写, 但平台 API 被命中 %d 次", got)
	}

	// 2) 开启每项目 PR 状态开关 → 应命中平台 API 一次。
	on := true
	if _, err := projSvc.Update(context.Background(), "proj-prstatus", project.UpdateInput{PRStatusEnabled: &on}); err != nil {
		t.Fatalf("Update flag on: %v", err)
	}
	hook(context.Background(), runID, run.StatusSuccess)
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("flag 开后应回写一次, 平台 API 命中 = %d, want 1", got)
	}
}

// TestPRStatusHookRefreshesExpiredOAuthToken 断言:回写提交状态前,过期的 OAuth access_token
// 会被保存的 refresh_token 静默续期(回写照常成功,凭据落库为新 token)。
func TestPRStatusHookRefreshesExpiredOAuthToken(t *testing.T) {
	// OAuth mock:授权码回 refresh_token;refresh_token 换轮换的新 access_token。
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("grant_type") == "refresh_token" {
			_, _ = w.Write([]byte(`{"access_token":"gho_REFRESHED_777","refresh_token":"rt_ROTATED_666"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"gho_STALE_111","refresh_token":"rt_STALE_222"}`))
	}))
	t.Cleanup(oauthSrv.Close)

	var hits int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(apiSrv.Close)

	st, err := store.Open(t.TempDir() + "/prstatus-refresh.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	v := vault.New(st.DB, testMasterKey())
	// OAuth 兑换得到的凭据:旧 access_token + 保存的 refresh_token(custom provider 前缀)。
	cred, err := v.Create(vault.CreateInput{
		Name: "custom:octocat", Type: vault.TypeGitToken,
		Secret: "gho_STALE_111", RefreshToken: "rt_STALE_222",
	})
	if err != nil {
		t.Fatalf("vault.Create: %v", err)
	}

	projSvc := project.New(st.DB, v, stubProberPR{})
	projID := "proj-prstatus-refresh"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.DB.Exec(
		`INSERT INTO projects (id, name, repo_url, default_branch, credential_id, pr_status_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
		projID, "demo", "https://github.com/acme/demo.git", "main", cred.ID, now, now,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	runSvc := run.New(st.DB)
	r, err := runSvc.Create(context.Background(), projID,
		run.Trigger{Type: run.TriggerManual, Branch: "main", Commit: "abcdef1234567890"})
	if err != nil {
		t.Fatalf("run.Create: %v", err)
	}

	// custom provider 注册 OAuth 应用(端点指向 oauth mock)并装配静默续期器。
	sec := "sec"
	oauthSvc := oauth.New(st.DB, v, oauthSrv.Client())
	if _, err := oauthSvc.SaveApp(context.Background(), oauth.SaveAppInput{
		Provider: oauth.ProviderCustom, ClientID: "cid", ClientSecret: &sec, BaseURL: oauthSrv.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveApp: %v", err)
	}
	refresher := gitrefresh.New(oauthSvc, v)

	reporter := prstatus.NewReporter(apiSrv.Client()).WithBaseURLs(apiSrv.URL, apiSrv.URL)
	hook := NewPRStatusHook(runSvc, projSvc, v, refresher, reporter, "https://pw.example.com", false)
	hook(context.Background(), r.ID, run.StatusSuccess)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("过期凭据回写应成功一次, hits = %d", got)
	}
	// 续期落库:旧 access_token 已被新 token 替换。
	got, err := v.Reveal(cred.ID)
	if err != nil || got != "gho_REFRESHED_777" {
		t.Fatalf("reveal after refresh = %q err=%v, want gho_REFRESHED_777", got, err)
	}
}

// TestPRStatusHookGlobalOverride 断言:flag 关但 globalOverride=true → 仍回写(向后兼容全局强开)。
func TestPRStatusHookGlobalOverride(t *testing.T) {
	hits, runID, _, projSvc, reporter, runSvc, v := prStatusHookFixture(t)

	// flag 默认关,但全局强开 → 应命中平台 API。
	hook := NewPRStatusHook(runSvc, projSvc, v, nil, reporter, "https://pw.example.com", true)
	hook(context.Background(), runID, run.StatusSuccess)
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("全局强开应无视每项目开关回写一次, 平台 API 命中 = %d, want 1", got)
	}
}
