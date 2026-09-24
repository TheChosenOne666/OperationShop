package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWeChatExchangeSuccess 验证成功换票：返回 openid 的账号键，且不含原文。
func TestWeChatExchangeSuccess(t *testing.T) {
	const openid = "oABC1234567890"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("js_code") != "code-xyz" || r.URL.Query().Get("grant_type") != "authorization_code" {
			t.Errorf("换票请求参数不符：%v", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		mustWrite(t, w, `{"openid":"`+openid+`","session_key":"sk-secret"}`)
	}))
	defer server.Close()
	exchanger := WeChatExchanger{AppID: "wx-test", AppSecret: "secret", Endpoint: server.URL}
	key, err := exchanger.Exchange(context.Background(), "code-xyz")
	if err != nil {
		t.Fatalf("换票失败：%v", err)
	}
	if key != AccountKey(openid) {
		t.Fatalf("账号键 = %q，期望 %q", key, AccountKey(openid))
	}
	if strings.Contains(key, openid) {
		t.Fatal("账号键不得包含 openid 原文")
	}
}

// TestWeChatExchangeRejections 验证各类拒绝：细节只进错误（供服务端日志），调用方一律报错。
func TestWeChatExchangeRejections(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		handler func(w http.ResponseWriter, r *http.Request)
	}{
		{"微信返回错误码", http.StatusOK, `{"errcode":40029,"errmsg":"invalid code"}`, nil},
		{"微信错误码为零但缺openid", http.StatusOK, `{"session_key":"sk"}`, nil},
		{"响应不是JSON", http.StatusOK, `<html>gateway</html>`, nil},
		{"网关错误", http.StatusBadGateway, `{}`, nil},
		{"空响应体", http.StatusOK, ``, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.handler != nil {
					tc.handler(w, r)
					return
				}
				w.WriteHeader(tc.status)
				mustWrite(t, w, tc.body)
			}))
			defer server.Close()
			exchanger := WeChatExchanger{AppID: "wx-test", AppSecret: "secret", Endpoint: server.URL}
			key, err := exchanger.Exchange(context.Background(), "code-xyz")
			if err == nil || key != "" {
				t.Fatalf("应拒绝：账号键 = %q，错误 = %v", key, err)
			}
		})
	}
	t.Run("网络不可达", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		endpoint := server.URL
		server.Close() // 立刻关掉，制造连接失败
		exchanger := WeChatExchanger{AppID: "wx-test", AppSecret: "secret", Endpoint: endpoint}
		if _, err := exchanger.Exchange(context.Background(), "code-xyz"); err == nil {
			t.Fatal("网络失败必须报错")
		}
	})
	t.Run("上下文取消", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		exchanger := WeChatExchanger{AppID: "wx-test", AppSecret: "secret", Endpoint: server.URL}
		if _, err := exchanger.Exchange(ctx, "code-xyz"); err == nil {
			t.Fatal("取消的上下文必须报错")
		}
	})
}

// TestWeChatExchangeRequiresCredentials 验证应用身份与 code 不能缺。
func TestWeChatExchangeRequiresCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("缺少身份的请求不应发到微信")
		mustWrite(t, w, `{}`)
	}))
	defer server.Close()
	cases := []struct {
		name      string
		exchanger WeChatExchanger
		code      string
	}{
		{"缺AppID", WeChatExchanger{AppSecret: "secret", Endpoint: server.URL}, "code-xyz"},
		{"缺Secret", WeChatExchanger{AppID: "wx-test", Endpoint: server.URL}, "code-xyz"},
		{"身份齐全但缺code", WeChatExchanger{AppID: "wx-test", AppSecret: "secret", Endpoint: server.URL}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.exchanger.Exchange(context.Background(), tc.code); err == nil {
				t.Fatalf("%s 必须被拒绝", tc.name)
			}
		})
	}
}

// TestDevExchange 验证开发假换票：合法 code 加前缀回声，拒绝可穿路径的 code。
func TestDevExchange(t *testing.T) {
	var exchanger DevExchanger
	key, err := exchanger.Exchange(context.Background(), "alice")
	if err != nil || key != "dev-alice" {
		t.Fatalf("合法开发 code 应加前缀回声：%q, %v", key, err)
	}
	if key2, _ := exchanger.Exchange(context.Background(), "bob"); key2 == key {
		t.Fatal("不同 code 必须得到不同账号键")
	}
	for _, code := range []string{"", "../dev-alice", "a/b", `a\b`, "空格 code", strings.Repeat("a", 65), "dev.alice"} {
		if _, err := exchanger.Exchange(context.Background(), code); err == nil {
			t.Fatalf("开发 code %q 必须被拒绝", code)
		}
	}
}

// TestDevExchangerSatisfiesInterface 保证两种换票实现可互换注入。
func TestDevExchangerSatisfiesInterface(t *testing.T) {
	var exchanger CodeExchanger = DevExchanger{}
	if _, err := exchanger.Exchange(context.Background(), "dev-alice"); err != nil {
		t.Fatalf("DevExchanger 应满足 CodeExchanger：%v", err)
	}
	var wechat CodeExchanger = WeChatExchanger{AppID: "wx", AppSecret: "s", Endpoint: "http://127.0.0.1:1"}
	if _, err := wechat.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("不可达端点必须报错")
	}
}

func mustWrite(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("写响应失败：%v", err)
	}
}
