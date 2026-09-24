// Command server starts the mall backend. MALL_AUTH_MODE selects the regime:
// "dev" (default) keeps the M01 prototype contract — loopback only, development
// token, one save file — with WeChat-style session logins available on top for
// rehearsing M07 flows; "wechat" serves real players over the internet with
// session tokens and one save file per account.
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
	"strconv"
	"strings"
	"time"

	"streetcorner/internal/api"
	"streetcorner/internal/game"
	"streetcorner/internal/session"
)

// defaultSessionTTL is short enough to exercise the expiry path during
// integration testing and long enough for a play session; raise it before launch.
const defaultSessionTTL = 10 * time.Minute

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	mode := api.Mode(strings.TrimSpace(os.Getenv("MALL_AUTH_MODE")))
	if mode == "" {
		mode = api.ModeDev
	}
	if mode != api.ModeDev && mode != api.ModeWechat {
		return fmt.Errorf("MALL_AUTH_MODE must be %q or %q, got %q", api.ModeDev, api.ModeWechat, string(mode))
	}
	required := []string{"MALL_LISTEN_ADDR", "MALL_CONFIG_PATH", "MALL_DATA_PATH"}
	if mode == api.ModeDev {
		required = append(required, "MALL_DEV_TOKEN")
	} else {
		required = append(required, "MALL_WECHAT_APPID", "MALL_WECHAT_APPSECRET")
	}
	for _, name := range required {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return fmt.Errorf("required environment variable is empty: %s", name)
		}
	}
	address := os.Getenv("MALL_LISTEN_ADDR")
	host, _, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return fmt.Errorf("MALL_LISTEN_ADDR must contain an explicit host and port")
	}
	// Dev mode keeps the loopback-only prototype contract; wechat mode must be
	// reachable from the internet, so any explicit bind address is accepted.
	if mode == api.ModeDev && !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("MALL_LISTEN_ADDR must contain an explicit loopback IP and port in dev mode")
	}
	cfg, err := game.LoadConfig(os.Getenv("MALL_CONFIG_PATH"))
	if err != nil {
		return err
	}
	dataPath := os.Getenv("MALL_DATA_PATH")
	ttl, err := sessionTTL()
	if err != nil {
		return err
	}
	var (
		primary   *game.Service
		accounts  *api.Accounts
		exchanger session.CodeExchanger
	)
	switch mode {
	case api.ModeDev:
		// The single development save keeps its exact file semantics; session
		// logins store their per-account saves in a sibling directory so the
		// prototype save is never touched by a logged-in account.
		service, err := game.NewService(cfg, game.FileStore{Path: dataPath}, time.Now)
		if err != nil {
			return err
		}
		primary = service
		accounts = api.NewAccounts(cfg, dataPath+".accounts", time.Now)
		exchanger = session.DevExchanger{}
	case api.ModeWechat:
		// MALL_DATA_PATH becomes a directory holding one save per account.
		if err := os.MkdirAll(dataPath, 0700); err != nil {
			return fmt.Errorf("create save directory: %w", err)
		}
		accounts = api.NewAccounts(cfg, dataPath, time.Now)
		exchanger = session.WeChatExchanger{AppID: os.Getenv("MALL_WECHAT_APPID"), AppSecret: os.Getenv("MALL_WECHAT_APPSECRET")}
	}
	sessions, err := session.NewStore(ttl, time.Now)
	if err != nil {
		return err
	}
	var origins []string
	if value := os.Getenv("MALL_ALLOWED_ORIGINS"); value != "" {
		for _, origin := range strings.Split(value, ",") {
			origins = append(origins, strings.TrimSpace(origin))
		}
	}
	handler, err := api.NewHandler(primary, api.Options{
		DevToken: os.Getenv("MALL_DEV_TOKEN"), AllowedOrigins: origins, Logger: logger,
		Mode: mode, Exchanger: exchanger, Sessions: sessions, Accounts: accounts, SessionTTL: ttl,
	})
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
	logger.Info("server started", "mode", string(mode), "address", listener.Addr().String(),
		"rules_version", cfg.RulesVersion, "wechat_login", mode == api.ModeWechat, "session_ttl_seconds", int64(ttl.Seconds()))
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

// sessionTTL reads MALL_SESSION_TTL (seconds); absent or invalid falls back to
// the default rather than guessing a duration from a partial value.
func sessionTTL() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv("MALL_SESSION_TTL"))
	if value == "" {
		return defaultSessionTTL, nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 || seconds > 86400 {
		return 0, fmt.Errorf("MALL_SESSION_TTL must be whole seconds between 1 and 86400")
	}
	return time.Duration(seconds) * time.Second, nil
}
