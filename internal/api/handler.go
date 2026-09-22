// Package api exposes the localhost-only prototype API, not production authentication.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"streetcorner/internal/game"
)

// Options contains the in-memory development token and explicitly allowed browser origins.
type Options struct {
	DevToken       string
	AllowedOrigins []string
	Logger         *slog.Logger
}

// NewHandler creates strict routes with loopback, origin, authentication and body checks.
func NewHandler(service *game.Service, options Options) (http.Handler, error) {
	if service == nil || len(options.DevToken) < 32 || options.Logger == nil {
		return nil, errors.New("service, logger and a development token of at least 32 characters are required")
	}
	allowed := make(map[string]bool)
	for _, origin := range options.AllowedOrigins {
		if origin == "" || origin == "*" || origin == "null" {
			return nil, errors.New("explicit non-wildcard browser origins are required")
		}
		allowed[origin] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": "local-development", "wechatLogin": false})
	})
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, service.Configuration())
	})
	mux.HandleFunc("GET /api/v1/mall", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, service.Snapshot())
	})
	mux.HandleFunc("POST /api/v1/shops/{id}/prepare", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := service.Prepare(r.PathValue("id"))
		respondOperation(w, r, options.Logger, result, err)
	})
	mux.HandleFunc("POST /api/v1/shops/{id}/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := service.Upgrade(r.PathValue("id"))
		respondOperation(w, r, options.Logger, result, err)
	})
	// Unlock addresses a slot id ("f2-s1"); shop commands address a shop id ("clothing").
	mux.HandleFunc("POST /api/v1/slots/{slotId}/unlock", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := service.Unlock(r.PathValue("slotId"))
		respondOperation(w, r, options.Logger, result, err)
	})
	mux.HandleFunc("POST /api/v1/mall/settle", func(w http.ResponseWriter, r *http.Request) {
		if !readEmptyCommand(w, r) {
			return
		}
		result, err := service.Settle()
		respondOperation(w, r, options.Logger, result, err)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Vary", "Origin")
		if !loopbackHost(r.Host) || !loopbackRemote(r.RemoteAddr) {
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
		if r.URL.Path != "/healthz" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(token), []byte(options.DevToken)) != 1 {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "需要有效的本地开发会话")
				return
			}
		}
		start := time.Now()
		mux.ServeHTTP(w, r)
		// Never log Authorization, payloads, query parameters or personal identifiers.
		options.Logger.Info("local API request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	}), nil
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
		logger.Info("local write command", "path", r.URL.Path, "code", code)
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
