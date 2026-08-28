package vault

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// pemKey 是一段以 PEM 头开头的伪 SSH 私钥(AC-SEC-01 用)。绝不可在库 dump 中出现。
const pemKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
SUPERSECRETPLAINTEXTMARKER1234567890abcdef
-----END OPENSSH PRIVATE KEY-----`

func testKey() *[keySize]byte {
	var k [keySize]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	return &k
}

func wrongKey() *[keySize]byte {
	var k [keySize]byte
	for i := range k {
		k[i] = byte(255 - i)
	}
	return &k
}

func testDB(t *testing.T) *sql.DB {
	return storetest.OpenDB(t)
}

// TestSealOpenRoundTrip 验证加解密往返 == 原文。
func TestSealOpenRoundTrip(t *testing.T) {
	key := testKey()
	plaintext := []byte(pemKey)
	sealed, err := seal(key, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(sealed) <= nonceSize {
		t.Fatalf("sealed too short: %d", len(sealed))
	}
	got, err := open(key, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != pemKey {
		t.Fatalf("round trip mismatch")
	}
}

// TestOpenWrongKeyFails 验证错误 master key 解密失败(认证标签校验)。
func TestOpenWrongKeyFails(t *testing.T) {
	sealed, err := seal(testKey(), []byte("hello world"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := open(wrongKey(), sealed); err == nil {
		t.Fatal("expected decrypt failure with wrong key, got nil")
	}
}

// TestNonceUnique 验证两次 seal 同一明文产生不同密文(随机 nonce)。
func TestNonceUnique(t *testing.T) {
	key := testKey()
	a, _ := seal(key, []byte("same"))
	b, _ := seal(key, []byte("same"))
	if string(a) == string(b) {
		t.Fatal("two seals produced identical ciphertext (nonce not random)")
	}
}

// TestUnconfiguredVault 验证未配置 master key 时所有操作返回 ErrVaultUnconfigured。
func TestUnconfiguredVault(t *testing.T) {
	v := New(testDB(t), nil)
	if _, err := v.Create(CreateInput{Name: "x", Type: TypeGitToken, Secret: "ghp_abc"}); err != ErrVaultUnconfigured {
		t.Fatalf("Create err = %v, want ErrVaultUnconfigured", err)
	}
	if _, err := v.List(); err != ErrVaultUnconfigured {
		t.Fatalf("List err = %v, want ErrVaultUnconfigured", err)
	}
	if _, err := v.Get("x"); err != ErrVaultUnconfigured {
		t.Fatalf("Get err = %v, want ErrVaultUnconfigured", err)
	}
	if _, err := v.Update("x", UpdateInput{}); err != ErrVaultUnconfigured {
		t.Fatalf("Update err = %v, want ErrVaultUnconfigured", err)
	}
	if err := v.Delete("x"); err != ErrVaultUnconfigured {
		t.Fatalf("Delete err = %v, want ErrVaultUnconfigured", err)
	}
	if _, _, err := v.RefreshContext("x"); err != ErrVaultUnconfigured {
		t.Fatalf("RefreshContext err = %v, want ErrVaultUnconfigured", err)
	}
	if err := v.RotateAccessToken("x", "new", "", ""); err != ErrVaultUnconfigured {
		t.Fatalf("RotateAccessToken err = %v, want ErrVaultUnconfigured", err)
	}
}

// TestRefreshContextRoundTrip 验证 OAuth 凭据保存 refresh_token 后可解密回读,
// 手动凭据(未附 refresh_token)返回空刷新上下文(非错误)。
func TestRefreshContextRoundTrip(t *testing.T) {
	v := New(testDB(t), testKey())
	cred, err := v.Create(CreateInput{
		Name: "gitee:octocat", Type: TypeGitToken, Scope: "global",
		Secret: "at_123", RefreshToken: "rt_secret_456",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	provider, rt, err := v.RefreshContext(cred.ID)
	if err != nil {
		t.Fatalf("RefreshContext: %v", err)
	}
	if provider != "gitee" || rt != "rt_secret_456" {
		t.Fatalf("refresh context: provider=%q rt=%q", provider, rt)
	}

	manual, _ := v.Create(CreateInput{Name: "manual", Type: TypeGitToken, Secret: "tok"})
	p2, rt2, err := v.RefreshContext(manual.ID)
	if err != nil || p2 != "" || rt2 != "" {
		t.Fatalf("manual refresh context should be empty: p=%q rt=%q err=%v", p2, rt2, err)
	}
	if _, _, err := v.RefreshContext("nope"); err != ErrNotFound {
		t.Fatalf("missing credential err = %v, want ErrNotFound", err)
	}
}

// TestRefreshContextProviderFromName 验证 provider 由凭据名前缀推导("provider:login")。
func TestRefreshContextProviderFromName(t *testing.T) {
	if got := oauthProviderFromName("gitee:octocat"); got != "gitee" {
		t.Fatalf("gitee prefix = %q", got)
	}
	if got := oauthProviderFromName("github:octocat"); got != "github" {
		t.Fatalf("github prefix = %q", got)
	}
	if got := oauthProviderFromName("custom:git.internal/foo"); got != "custom" {
		t.Fatalf("custom prefix = %q", got)
	}
	if got := oauthProviderFromName("manual token"); got != "" {
		t.Fatalf("no-colon name should yield empty provider, got %q", got)
	}
	if got := oauthProviderFromName(":leading-colon"); got != "" {
		t.Fatalf("leading-colon name should yield empty provider, got %q", got)
	}
}

// TestRotateAccessToken 验证续期后 access_token 与 refresh_token 双双轮换落库。
func TestRotateAccessToken(t *testing.T) {
	v := New(testDB(t), testKey())
	cred, _ := v.Create(CreateInput{
		Name: "gitee:octocat", Type: TypeGitToken, Scope: "global",
		Secret: "gho_at_old_123456789", RefreshToken: "rt_old", ExpiresAt: "2030-01-01T00:00:00Z",
	})
	newToken := "gho_at_rotated_new_987654321"
	if err := v.RotateAccessToken(cred.ID, newToken, "rt_new", ""); err != nil {
		t.Fatalf("RotateAccessToken: %v", err)
	}
	// 轮换未给新过期时间:既有 expires_at 保留(不清空判断依据)。
	if exp, _ := v.AccessExpiry(cred.ID); exp == nil || !exp.Equal(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry after rotate(no new value) = %v, want 2030-01-01 preserved", exp)
	}
	// 轮换带新过期时间:覆盖更新(access_token 不变,仅验证 expires_at 生效)。
	if err := v.RotateAccessToken(cred.ID, newToken, "rt_new", "2031-02-02T00:00:00Z"); err != nil {
		t.Fatalf("RotateAccessToken with new expiry: %v", err)
	}
	if exp, _ := v.AccessExpiry(cred.ID); exp == nil || !exp.Equal(time.Date(2031, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry after rotate(new value) = %v, want 2031-02-02", exp)
	}
	got, err := v.Reveal(cred.ID)
	if err != nil || got != newToken {
		t.Fatalf("reveal after rotate = %q err=%v", got, err)
	}
	_, rt, err := v.RefreshContext(cred.ID)
	if err != nil || rt != "rt_new" {
		t.Fatalf("refresh context after rotate = %q err=%v", rt, err)
	}
	// masked_value 同步更新(以新 token 末 4 位判定)。
	list, _ := v.List()
	if len(list) != 1 || !strings.HasSuffix(list[0].MaskedValue, "4321") {
		t.Fatalf("masked value not rotated: %+v", list)
	}
	// 空 access_token 拒绝轮换。
	if err := v.RotateAccessToken(cred.ID, "  ", "", ""); err != ErrEmptySecret {
		t.Fatalf("empty access token err = %v, want ErrEmptySecret", err)
	}

	// 刷新响应未携带新 refresh_token:既有 refresh_token 应保留,不能清空续期能力。
	if err := v.RotateAccessToken(cred.ID, "gho_at_rotated_no_rt", "", ""); err != nil {
		t.Fatalf("rotate without refresh_token: %v", err)
	}
	_, rt, err = v.RefreshContext(cred.ID)
	if err != nil || rt != "rt_new" {
		t.Fatalf("refresh token should be kept when response omits it, got %q err=%v", rt, err)
	}
}

// TestAccessExpiry 验证 access_token 过期时间(expires_at)的读回语义:
// 未配置/不存在/未知(nil)三种边界各归其位,供 gitrefresh 判断「过期/临期才刷新」。
func TestAccessExpiry(t *testing.T) {
	v := New(testDB(t), testKey())
	// 未知:创建未附 ExpiresAt → nil(存量凭据/手动创建),按「首次使用刷新并补记」处理。
	cred, _ := v.Create(CreateInput{Name: "gitee:octocat", Type: TypeGitToken, Secret: "at", RefreshToken: "rt"})
	if exp, err := v.AccessExpiry(cred.ID); err != nil || exp != nil {
		t.Fatalf("unknown expiry = %v err=%v, want nil", exp, err)
	}
	// 已知:创建带 ExpiresAt → 可读回原始值。
	cred2, _ := v.Create(CreateInput{Name: "gitee:bot", Type: TypeGitToken, Secret: "at2", ExpiresAt: "2030-06-15T08:30:00Z"})
	exp2, err := v.AccessExpiry(cred2.ID)
	want := time.Date(2030, 6, 15, 8, 30, 0, 0, time.UTC)
	if err != nil || exp2 == nil || !exp2.Equal(want) {
		t.Fatalf("expiry = %v err=%v, want %v", exp2, err, want)
	}
	// 不存在 → ErrNotFound。
	if _, err := v.AccessExpiry("missing-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
	// 未配置 master key → ErrVaultUnconfigured。
	uv := New(testDB(t), nil)
	if _, err := uv.AccessExpiry(cred.ID); err != ErrVaultUnconfigured {
		t.Fatalf("unconfigured err = %v, want ErrVaultUnconfigured", err)
	}
}

// TestUpdateRotateClearsRefreshToken 验证手动轮换 secret 时旧 refresh_token 一并失效清空,
// 防止续期拿到旧凭据的刷新上下文。
func TestUpdateRotateClearsRefreshToken(t *testing.T) {
	v := New(testDB(t), testKey())
	cred, _ := v.Create(CreateInput{
		Name: "gitee:octocat", Type: TypeGitToken, Secret: "at_old", RefreshToken: "rt_old",
	})
	newSecret := "at_manually_rotated"
	if _, err := v.Update(cred.ID, UpdateInput{Secret: &newSecret}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, rt, err := v.RefreshContext(cred.ID)
	if err != nil {
		t.Fatalf("RefreshContext: %v", err)
	}
	if rt != "" {
		t.Fatalf("manual rotation should clear refresh_token, got %q", rt)
	}
	got, _ := v.Reveal(cred.ID)
	if got != newSecret {
		t.Fatalf("secret after rotate = %q", got)
	}
}

// TestCreateGetRoundTrip 验证 Create→Get 取回原文,并更新 last_used_at。
func TestCreateGetRoundTrip(t *testing.T) {
	v := New(testDB(t), testKey())
	cred, err := v.Create(CreateInput{Name: "deploy key", Type: TypeSSHKey, Scope: "prod", Secret: pemKey})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cred.LastUsedAt != nil {
		t.Fatal("new credential should have nil lastUsedAt")
	}
	if strings.Contains(cred.MaskedValue, "SUPERSECRET") || strings.Contains(cred.MaskedValue, "BEGIN") {
		t.Fatalf("masked value leaks plaintext: %q", cred.MaskedValue)
	}

	plain, err := v.Get(cred.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if plain != pemKey {
		t.Fatal("Get did not return original plaintext")
	}

	// last_used_at 现已被更新。
	list, _ := v.List()
	if len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatal("lastUsedAt should be set after Get")
	}
}

// TestValidateType 验证类型枚举校验。
func TestValidateType(t *testing.T) {
	v := New(testDB(t), testKey())
	if _, err := v.Create(CreateInput{Name: "x", Type: "bogus", Secret: "s"}); err != ErrInvalidType {
		t.Fatalf("err = %v, want ErrInvalidType", err)
	}
	if _, err := v.Create(CreateInput{Name: "x", Type: TypeGitToken, Secret: ""}); err != ErrEmptySecret {
		t.Fatalf("err = %v, want ErrEmptySecret", err)
	}
	if _, err := v.Create(CreateInput{Name: "", Type: TypeGitToken, Secret: "s"}); err != ErrEmptyName {
		t.Fatalf("err = %v, want ErrEmptyName", err)
	}
}

// TestUpdateRotateSecret 验证轮换 secret 后旧密文换新、Get 返回新明文、掩码更新。
func TestUpdateRotateSecret(t *testing.T) {
	v := New(testDB(t), testKey())
	cred, _ := v.Create(CreateInput{Name: "tok", Type: TypeGitToken, Secret: "ghp_oldsecret1111"})

	newSecret := "ghp_newsecret9999"
	updated, err := v.Update(cred.ID, UpdateInput{Secret: &newSecret})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.HasSuffix(updated.MaskedValue, "9999") {
		t.Fatalf("masked not updated after rotation: %q", updated.MaskedValue)
	}
	plain, _ := v.Get(cred.ID)
	if plain != newSecret {
		t.Fatalf("Get after rotate = %q, want new secret", plain)
	}
}

// TestUpdateNameScope 验证仅改名/作用域不动密文。
func TestUpdateNameScope(t *testing.T) {
	v := New(testDB(t), testKey())
	cred, _ := v.Create(CreateInput{Name: "old", Type: TypeRegistry, Scope: "s1", Secret: "alice:pw"})
	newName, newScope := "new", "s2"
	updated, err := v.Update(cred.ID, UpdateInput{Name: &newName, Scope: &newScope})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "new" || updated.Scope != "s2" {
		t.Fatalf("update mismatch: %+v", updated)
	}
	plain, _ := v.Get(cred.ID)
	if plain != "alice:pw" {
		t.Fatal("secret changed unexpectedly")
	}
}

// TestDeleteNotFound 验证删除不存在的凭据返回 ErrNotFound。
func TestDeleteNotFound(t *testing.T) {
	v := New(testDB(t), testKey())
	if err := v.Delete("nope"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestGetNotFound 验证取不存在的凭据返回 ErrNotFound。
func TestGetNotFound(t *testing.T) {
	v := New(testDB(t), testKey())
	if _, err := v.Get("nope"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetGitAuthKeepsUsernameAndToken(t *testing.T) {
	v := New(testDB(t), testKey())
	cred, err := v.Create(CreateInput{
		Name: "gitee token", Type: TypeGitToken, Username: "actual-account", Secret: "token-value",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cred.Username != "actual-account" {
		t.Fatalf("created username = %q", cred.Username)
	}
	auth, err := v.GetGitAuth(cred.ID)
	if err != nil {
		t.Fatalf("GetGitAuth: %v", err)
	}
	if auth.Username != "actual-account" || auth.Token != "token-value" {
		t.Fatalf("GetGitAuth = %+v", auth)
	}
	listed, err := v.List()
	if err != nil || len(listed) != 1 || listed[0].Username != "actual-account" {
		t.Fatalf("List = %+v, err = %v", listed, err)
	}
}

// TestACSEC01_NoPlaintextInDB 是 AC-SEC-01 核心回归:
// 创建含 PEM 私钥的凭据后,遍历整库**所有表所有列** dump,grep 不到明文/PEM 头。
func TestACSEC01_NoPlaintextInDB(t *testing.T) {
	storetest.SkipIfMySQL(t) // 整库 dump 经 sqlite_master 遍历表,SQLite 文件专有;密文属性由 vault 层保证
	db := testDB(t)
	v := New(db, testKey())
	if _, err := v.Create(CreateInput{Name: "ci key", Type: TypeSSHKey, Scope: "prod", Secret: pemKey}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	dump := dumpEntireDB(t, db)
	forbidden := []string{
		"-----BEGIN",
		"SUPERSECRETPLAINTEXTMARKER1234567890abcdef",
		"OPENSSH PRIVATE KEY",
	}
	for _, needle := range forbidden {
		if strings.Contains(dump, needle) {
			t.Fatalf("AC-SEC-01 FAILED: DB dump contains forbidden plaintext %q", needle)
		}
	}
}

// dumpEntireDB 遍历所有用户表的所有列,把每个单元格(文本/BLOB)拼成一个大字符串。
func dumpEntireDB(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder

	tblRows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for tblRows.Next() {
		var name string
		if err := tblRows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	_ = tblRows.Close()

	for _, tbl := range tables {
		rows, err := db.Query(`SELECT * FROM "` + tbl + `"`)
		if err != nil {
			t.Fatalf("select * from %s: %v", tbl, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns %s: %v", tbl, err)
		}
		for rows.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan row %s: %v", tbl, err)
			}
			for _, c := range cells {
				switch val := c.(type) {
				case nil:
				case []byte:
					b.Write(val)
				case string:
					b.WriteString(val)
				default:
					// 数字/时间等:不可能含明文,跳过即可。
				}
				b.WriteByte('\n')
			}
		}
		_ = rows.Close()
	}
	return b.String()
}
