package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// codex-proxy-rs（下称 CPR）中继账号。
//
// 与 OAuth/apikey 的根本差别：出站的 TLS 指纹、身份头、environment_context 与
// user_location 全部由 CPR 负责，sub2api 只做用户体系、调度与展示。因此
// UsesOpenAICodexProtocol() 对本类型必须保持 false —— 那一整条指纹链
// （UA 强制、时区改写、zstd、chatgpt-account-id、req.Host=chatgpt.com）
// 都挂在它下面，一旦为真就会与 CPR 自己的改写打架。
//
// 每条本地记录代表一把已有 CPR 客户端 Key；权限、账号池与选号均由 CPR 管理。
const cprCredentialClientKey = "api_key"

var ErrCPRNotConfigured = errors.New("CPR global gateway or client API key is not configured")

// IsCPR 报告账号是否为 CPR 中继类型。
func (a *Account) IsCPR() bool {
	return a != nil && a.Type == AccountTypeCPR && a.Platform == PlatformOpenAI
}

func cloneCPRHTTPTransport(base http.RoundTripper) (*http.Transport, error) {
	transport, ok := base.(*http.Transport)
	if !ok || transport == nil {
		return nil, ErrCPRNotConfigured
	}
	return transport.Clone(), nil
}

func validateCPROutboundURL(cfg *config.Config, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if cfg == nil || err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "\r\n\t") {
		return "", ErrCPRNotConfigured
	}
	service := &OpenAIGatewayService{cfg: cfg}
	return service.validateUpstreamBaseURL(raw)
}

// validateCPRAccountShape 拒绝 platform ≠ openai 的 cpr 账号。
//
// IsCPR() 要求 platform 为 openai，但 GetCPRClientKey() 与 GetAccessToken 的
// case 分支都不要求。不拦的话可以造出 anthropic + cpr 这种账号：它在 anthropic
// 网关侧会 fail closed，但属于能落库的无效状态，排查时很费解。
func validateCPRAccountShape(platform, accountType string) error {
	if accountType == AccountTypeCPR && platform != PlatformOpenAI {
		return fmt.Errorf("cpr account type requires platform %q, got %q", PlatformOpenAI, platform)
	}
	return nil
}

// validatedCPRBaseURL 是 CPR 各出站构造器（Responses、透传、input_tokens、alpha search）共用的
// 全局地址守卫：缺地址直接报错，绝不回落到账号地址或官方端点。
func (s *OpenAIGatewayService) validatedCPRBaseURL(ctx context.Context, account *Account) (string, error) {
	if !account.IsCPR() {
		return "", ErrCPRNotConfigured
	}
	settings, err := s.GetCPRGateway(ctx)
	if err != nil {
		return "", err
	}
	if settings.BaseURL == "" {
		return "", ErrCPRNotConfigured
	}
	return validateCPROutboundURL(s.cfg, settings.BaseURL)
}

// GetCPRClientKey 返回用于 Authorization: Bearer 的 CPR client key。
func (a *Account) GetCPRClientKey() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.GetCredential(cprCredentialClientKey))
}
