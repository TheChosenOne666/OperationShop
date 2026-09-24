// Package api exposes the mall's HTTP API. Dev mode serves the single-account
// prototype (loopback only, development token); wechat mode authenticates
// WeChat session tokens and routes every request to its account's own save.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"streetcorner/internal/game"
	"streetcorner/internal/session"
)

// Mode selects the authentication regime. The zero value is ModeDev so that
// existing constructors keep the M01 prototype contract without changes.
type Mode string

const (
	// ModeDev keeps the prototype contract: loopback only, development token,
	// and (when configured) session logins on top for rehearsing M07 flows.
	ModeDev Mode = "dev"
	// ModeWechat serves real players: WeChat session tokens, no loopback limit,
	// one save per account.
	ModeWechat Mode = "wechat"
)

// Options configures the handler. Dev mode requires the service and a
// development token of at least 32 characters; wechat mode instead requires
// sessions, an exchanger and accounts, and rejects both the single-account
// service and any development token.
type Options struct {
	DevToken       string
	AllowedOrigins []string
	Logger         *slog.Logger
	Mode           Mode
	Exchanger      session.CodeExchanger
	Sessions       *session.Store
	Accounts       *Accounts
	// SessionTTL is echoed to clients as expiresInSeconds on login.
	SessionTTL time.Duration
}

// serviceContextKey carries the account service resolved by the middleware.
// Every route except /healthz and the login route reads it.
type serviceContextKey struct{}

// handler is the validated configuration shared by every route closure.
type handler struct {
	mode       Mode
	devToken   string
	allowed    map[string]bool
	logger     *slog.Logger
	primary    *game.Service
	exchanger  session.CodeExchanger
	sessions   *session.Store
	accounts   *Accounts
	sessionTTL time.Duration
}

// NewHandler creates strict routes. Dev mode preserves the M01 contract byte
// for byte; wechat mode drops the loopback limit and the development token and
// authenticates WeChat session tokens instead.
func NewHandler(service *game.Service, options Options) (http.Handler, error) {
	if options.Logger == nil {
		return nil, errors.New("a logger is required")
	}
	mode := options.Mode
	if mode == "" {
		mode = ModeDev
	}
	if mode != ModeDev && mode != ModeWechat {
		return nil, fmt.Errorf("unknown mode %q", string(options.Mode))
	}
	allowed := make(map[string]bool)
	for _, origin := range options.AllowedOrigins {
		if origin == "" || origin == "*" || origin == "null" {
			return nil, errors.New("explicit non-wildcard browser origins are required")
		}
		allowed[origin] = true
	}
	sessionsEnabled := options.Exchanger != nil || options.Sessions != nil || options.Accounts != nil
	switch mode {
	case ModeDev:
		if service == nil || len(options.DevToken) < 32 {
			return nil, errors.New("service and a development token of at least 32 characters are required")
		}
		// Session logins are optional in dev mode; when opened, all three parts
		// must be present so the login route cannot half-exist.
		if sessionsEnabled && (options.Exchanger == nil || options.Sessions == nil || options.Accounts == nil) {
			return nil, errors.New("session logins require an exchanger, a session store and accounts")
		}
	case ModeWechat:
		if service != nil || options.DevToken != "" {
			return nil, errors.New("wechat mode must not reuse the single-account service or a development token")
		}
		if options.Exchanger == nil || options.Sessions == nil || options.Accounts == nil {
			return nil, errors.New("wechat mode requires an exchanger, a session store and accounts")
		}
		// The development exchanger fabricates account keys from raw codes; in
		// wechat mode that would let anyone claim any account.
		if _, ok := options.Exchanger.(session.DevExchanger); ok {
			return nil, errors.New("the development exchanger is not allowed in wechat mode")
		}
	}
	h := &handler{
		mode: mode, devToken: options.DevToken, allowed: allowed, logger: options.Logger,
		primary: service, exchanger: options.Exchanger, sessions: options.Sessions,
		accounts: options.Accounts, sessionTTL: options.SessionTTL,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "mode": string(mode), "wechatLogin": mode == ModeWechat,
		})
	})
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, serviceFrom(r.Context()).Configuration())
	})
	mux.HandleFunc("GET /api/v1/mall", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, serviceFrom(r.Context()).Snapshot())
	})
	mux.HandleFunc("POST /api/v1/shops/{id}/prepare", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := serviceFrom(r.Context()).Prepare(r.PathValue("id"))
		respondOperation(w, r, h.logger, result, err)
	})
	mux.HandleFunc("POST /api/v1/shops/{id}/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := serviceFrom(r.Context()).Upgrade(r.PathValue("id"))
		respondOperation(w, r, h.logger, result, err)
	})
	// Unlock addresses a slot id ("f2-s1"); shop commands address a shop id ("clothing").
	mux.HandleFunc("POST /api/v1/slots/{slotId}/unlock", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := serviceFrom(r.Context()).Unlock(r.PathValue("slotId"))
		respondOperation(w, r, h.logger, result, err)
	})
	mux.HandleFunc("POST /api/v1/mall/settle", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := serviceFrom(r.Context()).Settle()
		respondOperation(w, r, h.logger, result, err)
	})
	// The login route exists only when session logins are configured. It is
	// public by design: the code is the credential, so no token precedes it.
	if sessionsEnabled {
		mux.HandleFunc("POST /api/v1/session/wechat", h.login)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Vary", "Origin")
		// Wechat mode serves players over the internet, so the loopback limit
		// and its LOCAL_ONLY error apply to dev mode only.
		if mode == ModeDev && (!loopbackHost(r.Host) || !loopbackRemote(r.RemoteAddr)) {
			writeError(w, http.StatusForbidden, "LOCAL_ONLY", "本服务仅供本机开发使用")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !allowed[origin] {
				writeError(w, http.StatusForbidden, "ORIGIN_DENIED", "浏览器来源未获允许")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/healthz" && r.URL.Path != "/api/v1/session/wechat" {
			account, ok := h.authenticate(w, r)
			if !ok {
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), serviceContextKey{}, account))
		}
		start := time.Now()
		mux.ServeHTTP(w, r)
		// Never log Authorization, payloads, query parameters or personal identifiers.
		h.logger.Info("api request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	}), nil
}

// authenticate resolves the account service for one request and writes the
// 401 response itself when the credentials do not qualify. A session token
// that aged out reports SESSION_EXPIRED so the client can log in again; in
// wechat mode any unknown token reports SESSION_INVALID, while dev mode keeps
// the prototype's UNAUTHORIZED for anything that is neither a live session
// nor the development token.
func (h *handler) authenticate(w http.ResponseWriter, r *http.Request) (*game.Service, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "需要有效的会话")
		return nil, false
	}
	if h.sessions != nil {
		accountKey, err := h.sessions.Authenticate(token)
		if err == nil {
			service, err := h.accounts.Service(accountKey)
			if err != nil {
				// A corrupt or incompatible save rejects this account only.
				h.logger.Error("account service unavailable", "error", err)
				writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "存档失败，本次操作未生效")
				return nil, false
			}
			return service, true
		}
		if errors.Is(err, session.ErrExpiredToken) {
			writeError(w, http.StatusUnauthorized, "SESSION_EXPIRED", "会话已过期，请重新进入游戏")
			return nil, false
		}
		// Unknown token: fall through — in dev mode it may still be the
		// development token, in wechat mode it ends as SESSION_INVALID below.
	}
	if h.mode == ModeDev && subtle.ConstantTimeCompare([]byte(token), []byte(h.devToken)) == 1 {
		return h.primary, true
	}
	if h.mode == ModeWechat {
		writeError(w, http.StatusUnauthorized, "SESSION_INVALID", "会话无效，请重新进入游戏")
	} else {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "需要有效的本地开发会话")
	}
	return nil, false
}

// login exchanges a WeChat login code for this server's own session token.
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	code, ok := readLoginCode(w, r)
	if !ok {
		return
	}
	accountKey, err := h.exchanger.Exchange(r.Context(), code)
	if err != nil {
		// WeChat's errcode/errmsg stay in the log; the client only sees the
		// stable code, so a failed exchange leaks no protocol detail.
		h.logger.Error("wechat code exchange failed", "error", err)
		writeError(w, http.StatusUnauthorized, "WECHAT_CODE_REJECTED", "微信登录失败，请重试")
		return
	}
	token, err := h.sessions.Issue(accountKey)
	if err != nil {
		h.logger.Error("session issue failed", "error", err)
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "登录失败，请重试")
		return
	}
	// The account key is a hash (or a validated dev code), never the openid.
	h.logger.Info("session issued", "account", accountKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expiresInSeconds": int64(h.sessionTTL.Seconds()),
	})
}

// readLoginCode strictly decodes {"code":"..."}: JSON content type, at most
// 1 KiB, exactly one JSON object whose only field is a non-empty code string.
func readLoginCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	if contentType := strings.Split(r.Header.Get("Content-Type"), ";")[0]; strings.TrimSpace(contentType) != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "JSON_REQUIRED", "请求类型必须是 application/json")
		return "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	var payload map[string]json.RawMessage
	if err := decoder.Decode(&payload); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "请求体超过上限")
		} else {
			writeError(w, http.StatusBadRequest, "INVALID_COMMAND", "请求体必须是包含 code 的 JSON 对象")
		}
		return "", false
	}
	if len(payload) != 1 {
		writeError(w, http.StatusBadRequest, "INVALID_COMMAND", "只接受 code 一个字段")
		return "", false
	}
	var code string
	if err := json.Unmarshal(payload["code"], &code); err != nil || code == "" {
		writeError(w, http.StatusBadRequest, "INVALID_COMMAND", "code 必须是非空字符串")
		return "", false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "请求体超过上限")
		} else {
			writeError(w, http.StatusBadRequest, "INVALID_COMMAND", "请求体必须只有一个 JSON 对象")
		}
		return "", false
	}
	return code, true
}

// serviceFrom returns the account service the middleware resolved. It is nil
// only on the two public routes, which never call it.
func serviceFrom(ctx context.Context) *game.Service {
	service, _ := ctx.Value(serviceContextKey{}).(*game.Service)
	return service
}

func readEmptyCommand(w http.ResponseWriter, r *http.Request) bool {
	if contentType := strings.Split(r.Header.Get("Content-Type"), ";")[0]; strings.TrimSpace(contentType) != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "JSON_REQUIRED", "请求类型必须是 application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	var payload map[string]json.RawMessage
	if err := decoder.Decode(&payload); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "请求体超过上限")
		} else {
			writeError(w, http.StatusBadRequest, "INVALID_COMMAND", "请求体必须是空 JSON 对象 {}")
		}
		return false
	}
	if payload == nil || len(payload) != 0 {
		writeError(w, http.StatusBadRequest, "INVALID_COMMAND", "不接受客户端金币、时间或客流数据；请发送 {}")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "请求体超过上限")
		} else {
			writeError(w, http.StatusBadRequest, "INVALID_COMMAND", "请求体必须只有一个 JSON 对象")
		}
		return false
	}
	return true
}

// respondOperation publishes one write command's outcome. B5.6 requires 结算 / 解锁 / 升级
// to be traceable by result code, and the generic access log only carries method, path and
// duration — so every outcome, including the accepted no-op, logs one stable code line.
func respondOperation(w http.ResponseWriter, r *http.Request, logger *slog.Logger, result game.Result, err error) {
	// Only path and the stable code are logged: never the payload, the token or an error
	// detail that would reach the log through a client-controlled value.
	logged := func(code string) {
		logger.Info("write command", "path", r.URL.Path, "code", code)
	}
	if err == nil {
		writeJSON(w, http.StatusOK, result)
		logged("OK")
		return
	}
	switch {
	case errors.Is(err, game.ErrUnknownShop):
		logged("SHOP_NOT_FOUND")
		writeError(w, http.StatusNotFound, "SHOP_NOT_FOUND", "店铺不存在")
	case errors.Is(err, game.ErrUnknownSlot):
		// An unknown slot id reuses the stable "target does not exist" code so the
		// client handles both id systems the same way.
		logged("SHOP_NOT_FOUND")
		writeError(w, http.StatusNotFound, "SHOP_NOT_FOUND", "铺位不存在")
	case errors.Is(err, game.ErrSlotLocked):
		logged("SLOT_LOCKED")
		writeError(w, http.StatusConflict, "SLOT_LOCKED", "该铺位尚未解锁")
	case errors.Is(err, game.ErrSlotOrder):
		logged("SLOT_ORDER")
		writeError(w, http.StatusConflict, "SLOT_ORDER", "必须按顺序解锁铺位")
	case errors.Is(err, game.ErrShopNotOpen):
		logged("SHOP_NOT_OPEN")
		writeError(w, http.StatusConflict, "SHOP_NOT_OPEN", "店铺尚未开业，不能升级")
	case errors.Is(err, game.ErrMaxLevel):
		logged("MAX_LEVEL")
		writeError(w, http.StatusConflict, "MAX_LEVEL", "店铺已满级")
	case errors.Is(err, game.ErrInsufficientCoins):
		logged("INSUFFICIENT_COINS")
		writeError(w, http.StatusConflict, "INSUFFICIENT_COINS", "金币不足")
	case errors.Is(err, game.ErrClockBackwards):
		logged("CLOCK_BACKWARDS")
		writeError(w, http.StatusConflict, "CLOCK_BACKWARDS", "服务端时间回退，暂不结算")
	case errors.Is(err, game.ErrNumericLimit):
		logged("NUMERIC_LIMIT")
		writeError(w, http.StatusConflict, "NUMERIC_LIMIT", "存档已达到安全数值上限")
	default:
		logger.Error("operation failed", "path", r.URL.Path, "error", err)
		logged("SAVE_FAILED")
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", "存档失败，本次操作未生效")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "response encoding failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		slog.Warn("response write failed", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func loopbackHost(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

func loopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	return err == nil && net.ParseIP(host).IsLoopback()
}
