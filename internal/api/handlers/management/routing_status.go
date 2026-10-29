package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GetRoutingStatus returns a side-effect-free snapshot of the current routing state.
func (h *Handler) GetRoutingStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	c.JSON(http.StatusOK, h.authManager.RoutingStatus())
}
