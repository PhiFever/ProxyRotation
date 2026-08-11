package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const Version = "0.1.0-dev"

const shutdownTimeout = 5 * time.Second

func main() {
	configPath := flag.String("c", "config.yaml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("ProxyRotation", Version)
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// 前置代理必须先于一切验证：它挂掉时每个上游都会"看起来"是死的——file 来源会
	// 整表探不通而拒绝启动，api / command 来源则每次失败都判定该换 IP，以最快速度烧钱。
	// 与其让故障伪装成上游问题，不如在这里直接停下。
	if cfg.Via != "" && !probeProxy("", cfg.Via, cfg.TestURL) {
		log.Fatalf("via: front proxy %s cannot reach test_url %s", cfg.Via, cfg.TestURL)
	}

	provider, err := NewProvider(cfg)
	if err != nil {
		log.Fatalf("provider: %v", err)
	}

	stats := NewStats(cfg.UnitPrice)
	rotator := NewRotator(cfg, provider, stats)

	proxySrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: NewProxyServer(cfg, rotator, stats),
	}
	adminSrv := &http.Server{
		Addr:    cfg.AdminListen,
		Handler: NewAdminHandler(rotator, stats),
	}

	go serve(proxySrv, "proxy")
	go serve(adminSrv, "admin")
	log.Printf("ProxyRotation %s: proxy on %s, admin on %s", Version, cfg.Listen, cfg.AdminListen)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = proxySrv.Shutdown(ctx)
	_ = adminSrv.Shutdown(ctx)
	log.Println("shutdown complete")
}

func serve(srv *http.Server, name string) {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("%s server: %v", name, err)
	}
}
