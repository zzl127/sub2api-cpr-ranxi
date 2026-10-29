package admin

import (
	"context"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Discovery is read-only. Provisioning is performed only inside account creation.
func (h *AccountHandler) DiscoverCPR(c *gin.Context) {
	var req struct {
		BaseURL      string `json:"base_url" binding:"required"`
		AdminBaseURL string `json:"admin_base_url"`
		AdminKey     string `json:"admin_api_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid CPR connection request")
		return
	}
	if h.accountUsageService == nil {
		response.BadRequest(c, "CPR integration unavailable")
		return
	}
	base := strings.TrimSpace(req.AdminBaseURL)
	if base == "" {
		base = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(req.BaseURL), "/"), "/v1")
	}
	accounts, err := h.accountUsageService.DiscoverCPR(c.Request.Context(), base, strings.TrimSpace(req.AdminKey))
	if err != nil {
		response.BadRequest(c, "CPR discovery failed: check the address and Admin Key")
		return
	}
	response.Success(c, accounts)
}

func (h *AccountHandler) configureCPR(ctx context.Context, platform string, credentials map[string]any) (map[string]any, error) {
	if platform != service.PlatformOpenAI || h.accountUsageService == nil {
		return nil, errors.New("CPR integration requires an OpenAI account")
	}
	copy := make(map[string]any, len(credentials)+1)
	for key, value := range credentials {
		copy[key] = value
	}
	a := &service.Account{Platform: platform, Type: service.AccountTypeCPR, Credentials: copy}
	key, err := h.accountUsageService.ConfigureCPRClient(ctx, a)
	if err != nil {
		return nil, errors.New("CPR one-to-one provisioning failed; check integration settings")
	}
	copy["api_key"] = key
	return copy, nil
}
