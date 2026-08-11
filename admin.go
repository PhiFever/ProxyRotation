package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewAdminHandler 只承载管理面。数据面走裸 net/http + io.Copy，
// 不让 gin 介入转发，避免框架开销与不必要的抽象。
func NewAdminHandler(r *Rotator, s *Stats) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New() // 不用 gin.Default()，避免默认日志中间件刷屏
	engine.Use(gin.Recovery())

	engine.GET("/stats", func(c *gin.Context) {
		c.JSON(http.StatusOK, s.Snapshot())
	})

	engine.POST("/switch", func(c *gin.Context) {
		proxy, err := r.Switch()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"current_proxy": proxy})
	})

	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	return engine
}
