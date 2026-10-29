package handler

import (
	"net/http"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CPRModels returns true only when it owns the response for a CPR-only group.
func (h *OpenAIGatewayHandler) CPRModels(c *gin.Context) bool {
	if h == nil {
		return false
	}
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || key.Group == nil || key.Group.Platform != service.PlatformOpenAI || h.gatewayService == nil {
		return false
	}
	enabled, err := h.gatewayService.CPRModelRelayGroup(c.Request.Context(), key.GroupID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "Model routing unavailable"}})
		return true
	}
	if !enabled {
		return false
	}
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		c.Request.Context(), key.GroupID, "", "", "", nil, service.OpenAIUpstreamTransportAny, "", false, false, false, service.PlatformOpenAI,
	)
	if err != nil || selection == nil || selection.Account == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "No CPR account available"}})
		return true
	}
	if !selection.Account.IsCPR() {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return true
	}
	c.Abort()
	started := false
	release, acquired := h.acquireResponsesAccountSlot(c, key.GroupID, "", selection, false, &started, requestLogger(c, "handler.cpr_models"))
	if acquired != openAISlotAcquireOK {
		return true
	}
	if release != nil {
		defer release()
	}
	if err := h.gatewayService.ForwardCPRModels(c.Request.Context(), c, selection.Account); err != nil && !c.Writer.Written() {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "CPR model discovery unavailable"}})
	}
	return true
}

// Waiting must not commit a synthetic 200/SSE preamble before CPR can return
// its own status and body. Mixed groups also retain that option until selection.
func (h *OpenAIGatewayHandler) CPRWireAdmission(c *gin.Context) {
	if h != nil && h.gatewayService != nil {
		key, ok := middleware2.GetAPIKeyFromContext(c)
		if ok && key.Group != nil && key.Group.Platform == service.PlatformOpenAI {
			found, err := h.gatewayService.CPRGroupHasAccount(c.Request.Context(), key.GroupID)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "CPR routing state unavailable"}})
				return
			}
			c.Set(service.CPRWireGroupKey, found)
		}
	}
	c.Next()
}
