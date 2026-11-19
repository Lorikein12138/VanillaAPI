package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func attachGatewayAdaptiveInput(c *gin.Context, body []byte) {
	ctx := service.SetGatewayAdaptiveInput(c.Request.Context(), body)
	if ctx != c.Request.Context() {
		c.Request = c.Request.WithContext(ctx)
	}
}
