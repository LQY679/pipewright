// Package gitrefresh 提供「OAuth access_token 过期时的静默续期」能力:
//
// 凭据保险库在 OAuth 授权码兑换时保存服务端返回的 refresh_token(仅 Gitee 等平台附带)。
// 各 git 读写入口(探测/克隆/读仓库文件/列分支等)在取凭据前调用 Refresher.MaybeRefresh:
// 若该凭据存有 refresh_token,则向 provider 的 tokenURL 发起 grant_type=refresh_token
// 请求换取新 access_token(及可能轮换的新 refresh_token)并加密落库,随后使用新 token。
// 全程无用户介入,access_token 过期不再导致克隆/探测失败。
//
// 设计约束:
//   - 刷新失败绝不阻塞主流程:调用方继续使用现有 token,其失效自然被后续鉴权失败暴露。
//   - 错误体与日志绝不含 access_token / refresh_token / client_secret 明文。
package gitrefresh

import (
	"context"
	"log"
	"time"

	"github.com/huangchengsir/pipewright/internal/oauth"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// Refresher 定义凭据静默续期能力。
type Refresher interface {
	// MaybeRefresh 对凭据做一次「有 refresh_token 即尝试续期」。
	// 返回 true 表示已用 refresh_token 换得新 access_token 并落库。
	// 无 refresh_token / 非 OAuth 凭据 / 保险库未配置 → 直接返回 (false, nil);
	// 续期失败 → 返回 false + err(调用方决定是否忽略)。
	MaybeRefresh(ctx context.Context, credentialID string) (bool, error)
}

// service 是依赖 oauth.Service + vault.Vault 的 Refresher 实现。
type service struct {
	oauth oauth.Service
	vault vault.Vault
}

// refreshAhead 是提前续期的安全余量:过期时间距今不足该窗口即视为「临期」并刷新。
const refreshAhead = 5 * time.Minute

// New 构造 Refresher。o 或 v 为 nil 时 MaybeRefresh 恒返回 (false, nil)(可安全空转)。
func New(o oauth.Service, v vault.Vault) Refresher {
	return &service{oauth: o, vault: v}
}

func (s *service) MaybeRefresh(ctx context.Context, credentialID string) (bool, error) {
	if credentialID == "" || s.oauth == nil || s.vault == nil {
		return false, nil
	}
	provider, refreshToken, err := s.vault.RefreshContext(credentialID)
	if err != nil {
		// 凭据不存在/保险库未配置/解密失败:不续期,不阻塞调用方。
		return false, nil
	}
	if provider == "" || refreshToken == "" {
		// 手动创建等非 OAuth 凭据:无 refresh_token,无需续期。
		return false, nil
	}
	// 仅「过期/临期/未知」才刷新:access_token 仍有效且未到临期窗口时直接跳过,
	// 避免每次 git 操作都向 provider 打刷新请求(限流/轮换踩踏风险)。
	exp, err := s.vault.AccessExpiry(credentialID)
	if err != nil {
		return false, nil
	}
	if exp != nil && time.Until(*exp) > refreshAhead {
		return false, nil
	}
	res, err := s.oauth.RefreshAccessToken(ctx, provider, refreshToken)
	if err != nil {
		return false, err
	}
	if res == nil || res.AccessToken == "" {
		return false, nil
	}
	// 平台返回 expires_in 时换算新的过期时间落库;未返回则保留既有值(存量凭据补记时仍为未知,
	// 后续按「每次使用刷新一次并补记」退化为保守策略,直至平台给出 expires_in)。
	var expiresAt string
	if res.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	if err := s.vault.RotateAccessToken(credentialID, res.AccessToken, res.RefreshToken, expiresAt); err != nil {
		return false, err
	}
	return true, nil
}

// Resolve 是「取可用的 git 认证」便捷函数:先尝试静默续期过期 token,再返回最新认证。
// refresher 为 nil 时等价于直接 v.GetGitAuth(向后兼容,调用方可不感知刷新)。
// 刷新失败仅记日志、不改变返回(调用方继续用现有 token)。
func Resolve(ctx context.Context, v vault.Vault, r Refresher, credentialID string) (vault.GitAuth, error) {
	if r != nil && credentialID != "" {
		refreshed, err := r.MaybeRefresh(ctx, credentialID)
		if err != nil {
			log.Printf("[gitrefresh] credential %s token refresh skipped: %v", credentialID, err)
		} else if refreshed {
			log.Printf("[gitrefresh] credential %s access_token refreshed", credentialID)
		}
	}
	return v.GetGitAuth(credentialID)
}
