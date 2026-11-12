// Package main 启动 repo-manager 仓库同步 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yanqin001/repo-manager/internal/manager"
)

func main() {
	filename := flag.String("config", "config/config.yaml", "部署配置的 YAML 路径")
	flag.Parse()
	if err := run(*filename); err != nil {
		slog.Error("repo-manager 退出", "error", err)
		os.Exit(1)
	}
}

func run(filename string) error {
	cfg, err := manager.LoadConfig(filename)
	if err != nil {
		return err
	}
	service, err := manager.New(cfg)
	if err != nil {
		return err
	}
	defer service.Close()

	srv := &http.Server{
		Addr: cfg.ListenAddr, Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	finished := make(chan error, 1)
	go func() {
		finished <- srv.ListenAndServe()
	}()
	slog.Info("repo-manager 启动", "listen_addr", cfg.ListenAddr, "local_repo_path", cfg.LocalRepoPath)
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
			return err
		}
		if err := <-finished; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
