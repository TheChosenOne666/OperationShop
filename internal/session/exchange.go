package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// CodeExchanger turns a one-time WeChat login code into an account key.
// Implementations must never leak the raw identifier: WeChatExchanger hashes
// the openid, DevExchanger echoes a validated development code.
type CodeExchanger interface {
	Exchange(ctx context.Context, code string) (string, error)
}

// WeChatCode2SessionEndpoint is the official jscode2session URL.
const WeChatCode2SessionEndpoint = "https://api.weixin.qq.com/sns/jscode2session"

// WeChatExchanger calls the real jscode2session API. AppSecret stays in the
// environment; the returned error carries WeChat's errcode/errmsg for the
// server log only — the API layer never forwards it to the client.
type WeChatExchanger struct {
	AppID     string
	AppSecret string
	Endpoint  string // empty means WeChatCode2SessionEndpoint
	HTTP      *http.Client
}

// code2SessionResponse mirrors the fields this service reads; WeChat's error
// shape (errcode/errmsg) shares the same envelope.
type code2SessionResponse struct {
	OpenID     string `json:"openid"`
	SessionKey string `json:"session_key"`
	UnionID    string `json:"unionid"`
	ErrCode    int    `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

// Exchange trades one login code for the account key of its WeChat user.
func (w WeChatExchanger) Exchange(ctx context.Context, code string) (string, error) {
	if w.AppID == "" || w.AppSecret == "" {
		return "", fmt.Errorf("wechat appid and secret are required")
	}
	if code == "" {
		return "", fmt.Errorf("a wechat login code is required")
	}
	endpoint := w.Endpoint
	if endpoint == "" {
		endpoint = WeChatCode2SessionEndpoint
	}
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	query := url.Values{}
	query.Set("appid", w.AppID)
	query.Set("secret", w.AppSecret)
	query.Set("js_code", code)
	query.Set("grant_type", "authorization_code")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("build code2session request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("call code2session: %w", err)
	}
	defer response.Body.Close()
	// The body is a small fixed envelope; cap it so a broken endpoint cannot
	// stream an unbounded response into memory.
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("read code2session response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("code2session returned http %d", response.StatusCode)
	}
	var parsed code2SessionResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode code2session response: %w", err)
	}
	if parsed.ErrCode != 0 {
		return "", fmt.Errorf("code2session rejected the code: errcode=%d errmsg=%q", parsed.ErrCode, parsed.ErrMsg)
	}
	if parsed.OpenID == "" {
		return "", fmt.Errorf("code2session response carries no openid")
	}
	return AccountKey(parsed.OpenID), nil
}

// devCodePattern keeps development codes inside a filename-safe alphabet: the
// derived account key becomes a save file name, so a code like "../../x" must
// be rejected at this boundary rather than sanitized downstream.
var devCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DevExchanger fabricates account keys from codes so the login flow can be
// exercised without WeChat. It must only ever be injected in dev mode; the API
// layer enforces that pairing (NewHandler rejects a DevExchanger in wechat mode).
type DevExchanger struct{}

// Exchange echoes a validated development code as the account key.
func (DevExchanger) Exchange(_ context.Context, code string) (string, error) {
	if !devCodePattern.MatchString(code) {
		return "", fmt.Errorf("development code must be 1-64 letters, digits, underscores or hyphens")
	}
	return "dev-" + code, nil
}

// ValidateAccountKey reports whether a key is safe to embed in a save file
// name. Both exchangers guarantee it; accounts.go re-checks at the filesystem
// boundary so a future exchanger cannot open a path-traversal hole by accident.
func ValidateAccountKey(key string) bool {
	return key != "" && len(key) <= 64 && !strings.ContainsAny(key, `/\`)
}
