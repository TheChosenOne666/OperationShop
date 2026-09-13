// Command server starts the local-only M01 backend. It does not authenticate WeChat users.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"streetcorner/internal/api"
	"streetcorner/internal/game"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	for _, name := range []string{"MALL_LISTEN_ADDR", "MALL_CONFIG_PATH", "MALL_DATA_PATH", "MALL_DEV_TOKEN"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return fmt.Errorf("required environment variable is empty: %s", name)
		}
	}
	address := os.Getenv("MALL_LISTEN_ADDR")
	host, _, err := net.SplitHostPort(address)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("MALL_LISTEN_ADDR must contain an explicit loopback IP and port")
	}
	cfg, err := game.LoadConfig(os.Getenv("MALL_CONFIG_PATH"))
	if err != nil {
		return err
	}
	service, err := game.NewService(cfg, game.FileStore{Path: os.Getenv("MALL_DATA_PATH")}, time.Now)
	if err != nil {
		return err
	}
	var origins []string
	if value := os.Getenv("MALL_ALLOWED_ORIGINS"); value != "" {
		for _, origin := range strings.Split(value, ",") {
			origins = append(origins, strings.TrimSpace(origin))
		}
	}
	handler, err := api.NewHandler(service, api.Options{DevToken: os.Getenv("MALL_DEV_TOKEN"), AllowedOrigins: origins, Logger: logger})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	completed := make(chan error, 1)
	go func() { completed <- server.Serve(listener) }()
	logger.Info("local development server started", "address", listener.Addr().String(), "rules_version", cfg.RulesVersion, "wechat_login", false)
	select {
	case err := <-completed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
