package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// These endpoints only access local settings. They never call CPR's admin API.
func (h *AccountHandler) GetCPRGateway(c *gin.Context) {
	settings, err := h.openAIGatewayService.GetCPRGateway(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) UpdateCPRGateway(c *gin.Context) {
	var req struct {
		BaseURL *string `json:"base_url" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid CPR gateway settings")
		return
	}
	settings, err := h.openAIGatewayService.SetCPRGateway(c.Request.Context(), service.CPRGatewaySettings{BaseURL: *req.BaseURL})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}
