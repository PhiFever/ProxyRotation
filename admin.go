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

	// switched=false 表示这次没换新的：要么撞上冷却窗口（并发 worker 挤在一起时
	// 的常态，返回的就是别人刚换好的那个），要么取新代理失败降级保留了旧的。
	// 调用方据此区分"换好了，可以重试"和"没换成，该退避"。
	engine.POST("/switch", func(c *gin.Context) {
		proxy, switched, err := r.Switch()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"current_proxy": proxy, "switched": switched})
	})

	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	return engine
}
