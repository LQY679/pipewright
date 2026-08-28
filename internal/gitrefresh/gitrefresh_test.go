package gitrefresh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/oauth"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

func testKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 3)
	}
	return &k
}

func strp(s string) *string { return &s }

// mockRefreshServer 模拟支持 refresh_token 授权码流的 provider(custom 推导 /oauth/token)。
func mockRefreshServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("grant_type") == "refresh_token" {
			_, _ = w.Write([]byte(`{"access_token":"gho_REFRESHED_777","refresh_token":"rt_ROTATED_666","expires_in":86400}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"gho_FAKE_111","refresh_token":"rt_FAKE_222"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newStack 组装 oauth.Service + vault.Vault + Refresher(真实 sqlite + 真实 oauth,仅 provider 为 mock)。
func newStack(t *testing.T) (oauth.Service, vault.Vault, Refresher, *httptest.Server) {
	t.Helper()
	server := mockRefreshServer(t)
	db := storetest.OpenDB(t)
	v := vault.New(db, testKey())
	svc := oauth.New(db, v, server.Client())
	// custom provider 按 baseURL 推导端点,配置 OAuth 应用。
	if _, err := svc.SaveApp(context.Background(), oauth.SaveAppInput{
		Provider: oauth.ProviderCustom, ClientID: "cid", ClientSecret: strp("sec"), BaseURL: server.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveApp: %v", err)
	}
	return svc, v, New(svc, v), server
}

func TestMaybeRefreshSkipsWhenNoRefreshToken(t *testing.T) {
	_, v, r, _ := newStack(t)
	manual, err := v.Create(vault.CreateInput{Name: "manual", Type: vault.TypeGitToken, Secret: "tok"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	refreshed, err := r.MaybeRefresh(context.Background(), manual.ID)
	if err != nil || refreshed {
		t.Fatalf("manual credential should not refresh: refreshed=%v err=%v", refreshed, err)
	}
	// 空 credentialID 也安全空转。
	if refreshed, err = r.MaybeRefresh(context.Background(), ""); err != nil || refreshed {
		t.Fatalf("empty id should no-op: refreshed=%v err=%v", refreshed, err)
	}
	_ = v
}

func TestMaybeRefreshRotatesExpiredToken(t *testing.T) {
	_, v, r, _ := newStack(t)
	cred, err := v.Create(vault.CreateInput{
		Name: oauth.ProviderCustom + ":octocat", Type: vault.TypeGitToken,
		Secret: "gho_FAKE_111", RefreshToken: "rt_FAKE_222",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	refreshed, err := r.MaybeRefresh(context.Background(), cred.ID)
	if err != nil {
		t.Fatalf("MaybeRefresh: %v", err)
	}
	if !refreshed {
		t.Fatal("OAuth credential with refresh_token should refresh")
	}
	// 落库新 token:后续克隆/探测直接用新 access_token。
	got, err := v.Reveal(cred.ID)
	if err != nil || got != "gho_REFRESHED_777" {
		t.Fatalf("reveal after refresh = %q err=%v", got, err)
	}
	_, rt, err := v.RefreshContext(cred.ID)
	if err != nil || rt != "rt_ROTATED_666" {
		t.Fatalf("refresh context after rotate = %q err=%v", rt, err)
	}
}

// TestMaybeRefreshSkipsWhenStillValid 断言:access_token 远未临期(有 expires_at 且未过期)
// 时不触发刷新(避免每次 git 操作都打 provider 刷新接口)。
func TestMaybeRefreshSkipsWhenStillValid(t *testing.T) {
	_, v, r, _ := newStack(t)
	cred, err := v.Create(vault.CreateInput{
		Name: oauth.ProviderCustom + ":octocat", Type: vault.TypeGitToken,
		Secret: "gho_FAKE_111", RefreshToken: "rt_FAKE_222",
		ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	refreshed, err := r.MaybeRefresh(context.Background(), cred.ID)
	if err != nil || refreshed {
		t.Fatalf("valid access token should skip refresh: refreshed=%v err=%v", refreshed, err)
	}
	got, _ := v.Reveal(cred.ID)
	if got != "gho_FAKE_111" {
		t.Fatalf("token changed unexpectedly: %q", got)
	}
}

// TestMaybeRefreshRecordsExpiryOnFirstUse 断言:存量凭据无 expires_at(未知)时首次使用
// 刷新一次,并把响应里的 expires_in 补记为过期时间;补记后仍在有效期内则不再刷新。
func TestMaybeRefreshRecordsExpiryOnFirstUse(t *testing.T) {
	_, v, r, _ := newStack(t)
	cred, _ := v.Create(vault.CreateInput{
		Name: oauth.ProviderCustom + ":octocat", Type: vault.TypeGitToken,
		Secret: "gho_FAKE_111", RefreshToken: "rt_FAKE_222",
	})
	refreshed, err := r.MaybeRefresh(context.Background(), cred.ID)
	if err != nil || !refreshed {
		t.Fatalf("unknown expiry should refresh once: refreshed=%v err=%v", refreshed, err)
	}
	// 刷新响应带 expires_in=86400 → 补记过期时间。
	exp, err := v.AccessExpiry(cred.ID)
	if err != nil || exp == nil {
		t.Fatalf("expiry should be recorded after first refresh, got %v err=%v", exp, err)
	}
	if d := exp.Sub(time.Now().Add(24 * time.Hour)); d < -time.Minute || d > time.Minute {
		t.Fatalf("recorded expiry %v not near now+24h (Δ %v)", exp, d)
	}
	// 补记后仍未临期 → 不再刷新。
	refreshed2, err := r.MaybeRefresh(context.Background(), cred.ID)
	if err != nil || refreshed2 {
		t.Fatalf("after recording expiry, second call should skip: refreshed=%v err=%v", refreshed2, err)
	}
}

func TestMaybeRefreshFailureDoesNotBlock(t *testing.T) {
	// provider 对 refresh_token 回 400 → 续期失败,但调用方可继续用旧 token(不 panic)。
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("grant_type") == "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"gho_FAKE_111","refresh_token":"rt_FAKE_222"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	db := storetest.OpenDB(t)
	v := vault.New(db, testKey())
	svc := oauth.New(db, v, srv.Client())
	if _, err := svc.SaveApp(context.Background(), oauth.SaveAppInput{
		Provider: oauth.ProviderCustom, ClientID: "cid", ClientSecret: strp("sec"), BaseURL: srv.URL, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveApp: %v", err)
	}
	r := New(svc, v)

	cred, _ := v.Create(vault.CreateInput{
		Name: oauth.ProviderCustom + ":octocat", Type: vault.TypeGitToken,
		Secret: "gho_FAKE_111", RefreshToken: "rt_FAKE_222",
	})
	refreshed, err := r.MaybeRefresh(context.Background(), cred.ID)
	if err == nil {
		t.Fatal("refresh failure should surface error")
	}
	if refreshed {
		t.Fatal("failed refresh must not report refreshed=true")
	}
	// 旧 token 原样保留。
	got, _ := v.Reveal(cred.ID)
	if got != "gho_FAKE_111" {
		t.Fatalf("old token changed unexpectedly: %q", got)
	}
}

func TestResolveRefreshesThenReturnsLatestAuth(t *testing.T) {
	_, v, r, _ := newStack(t)
	cred, _ := v.Create(vault.CreateInput{
		Name: oauth.ProviderCustom + ":octocat", Type: vault.TypeGitToken,
		Username: "octocat", Secret: "gho_FAKE_111", RefreshToken: "rt_FAKE_222",
	})
	auth, err := Resolve(context.Background(), v, r, cred.ID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if auth.Username != "octocat" || auth.Token != "gho_REFRESHED_777" {
		t.Fatalf("Resolve auth = %+v", auth)
	}
	// refresher 为 nil 时等价于直接 GetGitAuth(向后兼容)。
	auth2, err := Resolve(context.Background(), v, nil, cred.ID)
	if err != nil || auth2.Token != "gho_REFRESHED_777" {
		t.Fatalf("Resolve without refresher = %+v err=%v", auth2, err)
	}
}
