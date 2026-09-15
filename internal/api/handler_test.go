package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"streetcorner/internal/game"
)

const (
	testDevToken   = "local-development-token-0123456789"
	testOrigin     = "http://127.0.0.1:7456"
	testHost       = "127.0.0.1:8080"
	testRemoteAddr = "127.0.0.1:54321"
	leakSecret     = "SECRETCODE"
	// testMaxSafeInteger 与 game 包内的安全整数上限保持一致。
	testMaxSafeInteger = 1<<53 - 1
)

func testTime() time.Time {
	return time.Date(2026, time.September, 12, 4, 0, 0, 0, time.UTC)
}

type stubClock struct {
	mu    sync.Mutex
	stamp time.Time
}

func (c *stubClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stamp
}

func (c *stubClock) set(stamp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stamp = stamp
}

// stubStore 是测试用的内存存档，用于注入读写错误与边界状态。
type stubStore struct {
	mu      sync.Mutex
	state   game.State
	exists  bool
	loadErr error
	saveErr error
	saves   int
}

// Load 返回内存存档副本，或注入的读取错误。
func (s *stubStore) Load() (game.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return game.State{}, s.loadErr
	}
	if !s.exists {
		return game.State{}, os.ErrNotExist
	}
	return copyState(s.state), nil
}

// Save 保存输入的副本；注入错误时保留原有存档。
func (s *stubStore) Save(state game.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.state, s.exists = copyState(state), true
	return nil
}

func (s *stubStore) failSave(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveErr = err
}

func (s *stubStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

func (s *stubStore) storedState() game.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyState(s.state)
}

// seed 在已初始化的存档上应用变更，用于构造边界状态。
func (s *stubStore) seed(t *testing.T, mutate func(*game.State)) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exists {
		t.Fatal("必须先初始化存档才能改写")
	}
	mutate(&s.state)
}

func copyState(state game.State) game.State {
	state.UnlockedSlots = append([]string(nil), state.UnlockedSlots...)
	shops := state.Shops
	state.Shops = make([]game.ShopState, len(shops))
	for i, shop := range shops {
		shop.Segments = append([]game.Segment(nil), shop.Segments...)
		state.Shops[i] = shop
	}
	return state
}

// syncBuffer 让并发请求的日志写入不产生数据竞争。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func loadTestConfig(t *testing.T) game.Config {
	t.Helper()
	cfg, err := game.LoadConfig(filepath.Join("..", "..", "config", "development.json"))
	if err != nil {
		t.Fatalf("读取开发配置失败：%v", err)
	}
	return cfg
}

// fundedConfig 只调高初始金币，便于在接口层走完解锁与升级路径。
func fundedConfig(t *testing.T) game.Config {
	t.Helper()
	cfg := loadTestConfig(t)
	cfg.InitialCoins = 100_000
	return cfg
}

func newTestService(t *testing.T) *game.Service {
	t.Helper()
	clock := &stubClock{stamp: testTime()}
	service, err := game.NewService(loadTestConfig(t), &stubStore{}, clock.now)
	if err != nil {
		t.Fatalf("创建本地开发服务失败：%v", err)
	}
	return service
}

// testAPI 组装一套处理器、服务、存档与时钟，便于逐层断言。
type testAPI struct {
	t       *testing.T
	token   string
	origins []string
	config  game.Config
	store   *stubStore
	clock   *stubClock
	logs    *syncBuffer
	logger  *slog.Logger
	service *game.Service
	handler http.Handler
}

func newTestAPIWith(t *testing.T, token string, origins []string) *testAPI {
	t.Helper()
	return newTestAPIWithConfig(t, token, origins, loadTestConfig(t))
}

func newTestAPIWithConfig(t *testing.T, token string, origins []string, cfg game.Config) *testAPI {
	t.Helper()
	a := &testAPI{
		t: t, token: token, origins: append([]string(nil), origins...), config: cfg,
		store: &stubStore{}, clock: &stubClock{stamp: testTime()}, logs: &syncBuffer{},
	}
	a.logger = slog.New(slog.NewJSONHandler(a.logs, nil))
	a.start()
	return a
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	return newTestAPIWith(t, testDevToken, []string{testOrigin})
}

// newFundedTestAPI 使用金币充足的配置，便于走完解锁与升级路径。
func newFundedTestAPI(t *testing.T) *testAPI {
	t.Helper()
	return newTestAPIWithConfig(t, testDevToken, []string{testOrigin}, fundedConfig(t))
}

// start 使用当前存档与时钟重建服务与处理器，用于模拟进程重启。
func (a *testAPI) start() {
	a.t.Helper()
	service, err := game.NewService(a.config, a.store, a.clock.now)
	if err != nil {
		a.t.Fatalf("创建本地开发服务失败：%v", err)
	}
	handler, err := NewHandler(service, Options{DevToken: a.token, AllowedOrigins: a.origins, Logger: a.logger})
	if err != nil {
		a.t.Fatalf("创建处理器失败：%v", err)
	}
	a.service, a.handler = service, handler
}

// newRequest 构造默认本机且已认证的请求，mutate 可覆盖默认请求头。
func (a *testAPI) newRequest(method, target, body string, mutate ...func(*http.Request)) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Host = testHost
	request.RemoteAddr = testRemoteAddr
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+a.token)
	for _, fn := range mutate {
		fn(request)
	}
	return request
}

func (a *testAPI) call(method, target, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, a.newRequest(method, target, body, mutate...))
	return recorder
}

func dropAuth(request *http.Request) {
	request.Header.Del("Authorization")
}

func assertStatus(t *testing.T, recorder *httptest.ResponseRecorder, status int) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("状态码 = %d，期望 %d（响应体 %q）", recorder.Code, status, recorder.Body.String())
	}
}

func assertSecureHeaders(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	header := recorder.Header()
	if header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Cache-Control") != "no-store" ||
		header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Vary") != "Origin" {
		t.Fatalf("安全响应头不符：%v", header)
	}
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func assertErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) errorEnvelope {
	t.Helper()
	assertStatus(t, recorder, status)
	assertSecureHeaders(t, recorder)
	var envelope errorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析错误响应失败：%v（响应体 %q）", err, recorder.Body.String())
	}
	if envelope.Error.Code != code || envelope.Error.Message == "" {
		t.Fatalf("错误响应 = %+v，期望代码 %q", envelope, code)
	}
	return envelope
}

func decodeBody[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("解析响应失败：%v（响应体 %q）", err, recorder.Body.String())
	}
	return value
}

// TestNewHandlerRejectsInvalidOptions 验证服务、日志、令牌及来源必须显式合法。
func TestNewHandlerRejectsInvalidOptions(t *testing.T) {
	service := newTestService(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name    string
		service *game.Service
		options Options
	}{
		{"缺少服务", nil, Options{DevToken: testDevToken, Logger: logger}},
		{"缺少日志", service, Options{DevToken: testDevToken}},
		{"缺少令牌", service, Options{Logger: logger}},
		{"令牌不足三十二位", service, Options{DevToken: strings.Repeat("a", 31), Logger: logger}},
		{"通配来源", service, Options{DevToken: testDevToken, Logger: logger, AllowedOrigins: []string{"*"}}},
		{"空来源", service, Options{DevToken: testDevToken, Logger: logger, AllowedOrigins: []string{""}}},
		{"null来源", service, Options{DevToken: testDevToken, Logger: logger, AllowedOrigins: []string{"null"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewHandler(tc.service, tc.options)
			if handler != nil || err == nil {
				t.Fatalf("非法选项必须拒绝：%v，%v", handler, err)
			}
		})
	}
	handler, err := NewHandler(service, Options{DevToken: testDevToken, AllowedOrigins: []string{testOrigin}, Logger: logger})
	if handler == nil || err != nil {
		t.Fatalf("合法选项应创建处理器：%v", err)
	}
	handler, err = NewHandler(service, Options{DevToken: testDevToken, Logger: logger})
	if handler == nil || err != nil {
		t.Fatalf("不放开任何浏览器来源也应可用：%v", err)
	}
}

// TestHealthzIsPublic 验证健康检查无需令牌且不触碰存档。
func TestHealthzIsPublic(t *testing.T) {
	a := newTestAPI(t)
	recorder := a.call(http.MethodGet, "/healthz", "", dropAuth)
	assertStatus(t, recorder, http.StatusOK)
	assertSecureHeaders(t, recorder)
	want := map[string]any{"status": "ok", "mode": "local-development", "wechatLogin": false}
	if got := decodeBody[map[string]any](t, recorder); !reflect.DeepEqual(got, want) {
		t.Fatalf("健康检查响应 = %+v，期望 %+v", got, want)
	}
	if a.store.saveCount() != 1 {
		t.Fatalf("只读健康检查不应写入存档，实际保存 %d 次", a.store.saveCount())
	}
}

// TestLoopbackEnforcement 验证仅接受本机主机头与本机来源地址。
func TestLoopbackEnforcement(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		remote     string
		wantStatus int
		wantCode   string
	}{
		{"回环地址", "127.0.0.1:8080", "127.0.0.1:54321", http.StatusOK, ""},
		{"localhost主机名", "localhost:8080", "127.0.0.1:54321", http.StatusOK, ""},
		{"IPv6回环", "[::1]:8080", "[::1]:54321", http.StatusOK, ""},
		{"外部主机头", "mall.example.com:8080", "127.0.0.1:54321", http.StatusForbidden, "LOCAL_ONLY"},
		{"外部主机头无端口", "mall.example.com", "127.0.0.1:54321", http.StatusForbidden, "LOCAL_ONLY"},
		{"外部来源地址", "127.0.0.1:8080", "203.0.113.9:51000", http.StatusForbidden, "LOCAL_ONLY"},
		{"来源地址缺少端口", "127.0.0.1:8080", "127.0.0.1", http.StatusForbidden, "LOCAL_ONLY"},
		{"通配绑定地址", "0.0.0.0:8080", "127.0.0.1:54321", http.StatusForbidden, "LOCAL_ONLY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAPI(t)
			recorder := a.call(http.MethodGet, "/api/v1/mall", "", func(request *http.Request) {
				request.Host, request.RemoteAddr = tc.host, tc.remote
			})
			if tc.wantCode == "" {
				assertStatus(t, recorder, tc.wantStatus)
			} else {
				assertErrorCode(t, recorder, tc.wantStatus, tc.wantCode)
			}
			if a.store.saveCount() != 1 {
				t.Fatal("受限请求不得写入存档")
			}
		})
	}
}

// TestAuthenticationRequired 验证除健康检查外的全部路由都要求 Bearer 令牌。
func TestAuthenticationRequired(t *testing.T) {
	endpoints := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, "/api/v1/config", ""},
		{http.MethodGet, "/api/v1/mall", ""},
		{http.MethodPost, "/api/v1/shops/coffee/prepare", "{}"},
		{http.MethodPost, "/api/v1/shops/coffee/upgrade", "{}"},
		{http.MethodPost, "/api/v1/slots/f2-s1/unlock", "{}"},
		{http.MethodPost, "/api/v1/mall/settle", "{}"},
	}
	headers := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"缺少认证头", dropAuth},
		{"缺少Bearer前缀", func(r *http.Request) { r.Header.Set("Authorization", testDevToken) }},
		{"小写bearer", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+testDevToken) }},
		{"令牌为空", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }},
		{"令牌错误", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("b", len(testDevToken))) }},
		{"令牌更长", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testDevToken+"x") }},
		{"令牌更短", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testDevToken[:31]) }},
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint.method+" "+endpoint.target, func(t *testing.T) {
			a := newTestAPI(t)
			for _, header := range headers {
				saves := a.store.saveCount()
				recorder := a.call(endpoint.method, endpoint.target, endpoint.body, header.mutate)
				assertErrorCode(t, recorder, http.StatusUnauthorized, "UNAUTHORIZED")
				if a.store.saveCount() != saves {
					t.Fatalf("%s 被拒绝的请求不得写入存档", header.name)
				}
			}
			// 携带合法令牌时不得再被认证拦下（业务结果可能是 409 等状态冲突）。
			if recorder := a.call(endpoint.method, endpoint.target, endpoint.body); recorder.Code == http.StatusUnauthorized {
				t.Fatalf("合法令牌不应被认证拦下：%d", recorder.Code)
			}
		})
	}
}

// TestOriginPolicy 验证浏览器来源白名单、预检响应与来源拒绝。
func TestOriginPolicy(t *testing.T) {
	a := newTestAPI(t)
	t.Run("无来源", func(t *testing.T) {
		recorder := a.call(http.MethodGet, "/api/v1/mall", "")
		assertStatus(t, recorder, http.StatusOK)
		if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("无来源请求不应返回跨域许可")
		}
	})
	t.Run("允许来源", func(t *testing.T) {
		recorder := a.call(http.MethodGet, "/api/v1/mall", "", func(r *http.Request) { r.Header.Set("Origin", testOrigin) })
		assertStatus(t, recorder, http.StatusOK)
		header := recorder.Header()
		if header.Get("Access-Control-Allow-Origin") != testOrigin ||
			header.Get("Access-Control-Allow-Methods") != "GET, POST, OPTIONS" ||
			header.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" {
			t.Fatalf("跨域响应头不符：%v", header)
		}
	})
	t.Run("预检请求", func(t *testing.T) {
		recorder := a.call(http.MethodOptions, "/api/v1/mall/settle", "", func(r *http.Request) {
			r.Header.Set("Origin", testOrigin)
			r.Header.Set("Access-Control-Request-Method", http.MethodPost)
		})
		assertStatus(t, recorder, http.StatusNoContent)
		if recorder.Body.Len() != 0 || recorder.Header().Get("Access-Control-Allow-Origin") != testOrigin {
			t.Fatalf("预检响应不符：%q，%v", recorder.Body.String(), recorder.Header())
		}
	})
	t.Run("无来源预检", func(t *testing.T) {
		recorder := a.call(http.MethodOptions, "/api/v1/mall/settle", "")
		assertStatus(t, recorder, http.StatusNoContent)
	})
	for _, origin := range []string{"http://evil.example.com", "null", "http://127.0.0.1:7456/", "HTTP://127.0.0.1:7456", "http://127.0.0.1:7456.evil.com"} {
		t.Run("拒绝来源 "+origin, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodOptions} {
				recorder := a.call(method, "/api/v1/mall", "", func(r *http.Request) { r.Header.Set("Origin", origin) })
				assertErrorCode(t, recorder, http.StatusForbidden, "ORIGIN_DENIED")
				if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Fatal("被拒绝的来源不应返回跨域许可")
				}
			}
		})
	}
	if a.store.saveCount() != 1 {
		t.Fatal("来源校验不得触达业务逻辑")
	}
}

// TestReadRoutes 验证配置与商城快照接口返回服务端权威数据与派生字段。
func TestReadRoutes(t *testing.T) {
	a := newTestAPI(t)
	recorder := a.call(http.MethodGet, "/api/v1/config", "")
	assertStatus(t, recorder, http.StatusOK)
	assertSecureHeaders(t, recorder)
	if got := decodeBody[game.Config](t, recorder); !reflect.DeepEqual(got, a.config) {
		t.Fatalf("配置响应与服务端规则不一致：%+v", got)
	}
	recorder = a.call(http.MethodGet, "/api/v1/mall", "")
	assertStatus(t, recorder, http.StatusOK)
	view := decodeBody[game.MallView](t, recorder)
	if !reflect.DeepEqual(view, a.service.Snapshot()) {
		t.Fatalf("快照响应与服务端状态不一致：%+v", view)
	}
	// 未冻结的当天：接口必须返回 null 上限，客户端永远看不到占位 0。
	if view.CapFrozen || view.DailyVisitorCap != nil || view.VisitorsServed != 0 {
		t.Fatalf("未冻结时 dailyVisitorCap 必须为 null：%+v", view)
	}
	if view.NextSlotID == nil || view.NextUnlockCost == nil {
		t.Fatalf("必须给出下一可解锁铺位：%+v", view)
	}
	for i, shop := range view.Shops {
		if shop.Level != 1 || shop.UpgradeCost == nil || *shop.UpgradeCost != a.config.UpgradeCosts[0] {
			t.Fatalf("店铺视图字段错误：%+v", shop)
		}
		if shop.UnitPrice != a.config.Shops[i].LevelCoinsPerVisitor[0] {
			t.Fatalf("未满铺时单价应为等级单价：%+v", shop)
		}
	}
}

// TestPrepareAndSettleRoutes 验证命令路由的幂等、结算与未知店铺处理。
func TestPrepareAndSettleRoutes(t *testing.T) {
	a := newTestAPI(t)
	recorder := a.call(http.MethodPost, "/api/v1/shops/coffee/prepare", "{}")
	assertStatus(t, recorder, http.StatusOK)
	prepared := decodeBody[game.Result](t, recorder)
	if !prepared.Changed || prepared.State.Revision != 2 || !prepared.State.Shops[0].Prepared {
		t.Fatalf("首次准备结果不符：%+v", prepared)
	}
	if prepared.State.CapFrozen || prepared.State.DailyVisitorCap != nil {
		t.Fatalf("开店本身不得冻结客流上限：%+v", prepared.State)
	}
	recorder = a.call(http.MethodPost, "/api/v1/shops/coffee/prepare", "{}")
	assertStatus(t, recorder, http.StatusOK)
	repeated := decodeBody[game.Result](t, recorder)
	if repeated.Changed || !reflect.DeepEqual(repeated.State, prepared.State) || a.store.saveCount() != 2 {
		t.Fatalf("重复准备必须无副作用：%+v，保存 %d 次", repeated, a.store.saveCount())
	}
	price := a.config.Shops[0].LevelCoinsPerVisitor[0]
	a.clock.set(testTime().Add(5 * time.Second))
	recorder = a.call(http.MethodPost, "/api/v1/mall/settle", "{}")
	assertStatus(t, recorder, http.StatusOK)
	settled := decodeBody[game.Result](t, recorder)
	if !settled.Changed || settled.EarnedCoins != price || settled.VisitorsUsed != 1 {
		t.Fatalf("结算结果不符：%+v", settled)
	}
	if settled.State.DailyVisitorCap == nil || *settled.State.DailyVisitorCap != a.config.BaseVisitors+a.config.VisitorsPerShop ||
		settled.State.VisitorsServed != 1 || settled.State.VisitorsRemaining != *settled.State.DailyVisitorCap-1 {
		t.Fatalf("首次产生客人后必须冻结上限并给出已到店数：%+v", settled.State)
	}
	before := a.service.Snapshot()
	saves := a.store.saveCount()
	recorder = a.call(http.MethodPost, "/api/v1/shops/unknown/prepare", "{}")
	assertErrorCode(t, recorder, http.StatusNotFound, "SHOP_NOT_FOUND")
	recorder = a.call(http.MethodPost, "/api/v1/shops/Coffee/prepare", "{}")
	assertErrorCode(t, recorder, http.StatusNotFound, "SHOP_NOT_FOUND")
	if !reflect.DeepEqual(a.service.Snapshot(), before) || a.store.saveCount() != saves {
		t.Fatal("未知店铺请求不得触发结算或保存")
	}
}

// TestUnlockAndUpgradeRoutes 验证解锁铺位与升级店铺两条新命令路由。
func TestUnlockAndUpgradeRoutes(t *testing.T) {
	a := newFundedTestAPI(t)
	slot := a.config.Slots[2] // 第一个二层铺位
	shopIndex := 2
	recorder := a.call(http.MethodPost, "/api/v1/slots/"+slot.ID+"/unlock", "{}")
	assertStatus(t, recorder, http.StatusOK)
	unlocked := decodeBody[game.Result](t, recorder)
	if !unlocked.Changed || unlocked.State.Coins != a.config.InitialCoins-slot.UnlockCost || unlocked.State.Spent != slot.UnlockCost {
		t.Fatalf("解锁结果不符：%+v", unlocked)
	}
	if !contains(unlocked.State.UnlockedSlots, slot.ID) {
		t.Fatalf("解锁后必须出现在 unlockedSlots 中：%v", unlocked.State.UnlockedSlots)
	}
	// 重复解锁：幂等成功、不写存档。
	saves := a.store.saveCount()
	recorder = a.call(http.MethodPost, "/api/v1/slots/"+slot.ID+"/unlock", "{}")
	assertStatus(t, recorder, http.StatusOK)
	repeated := decodeBody[game.Result](t, recorder)
	if repeated.Changed || a.store.saveCount() != saves {
		t.Fatalf("重复解锁必须幂等且不写存档：%+v", repeated)
	}
	// 解锁后开店，再升级。
	assertStatus(t, a.call(http.MethodPost, "/api/v1/shops/"+slot.ShopID+"/prepare", "{}"), http.StatusOK)
	recorder = a.call(http.MethodPost, "/api/v1/shops/"+slot.ShopID+"/upgrade", "{}")
	assertStatus(t, recorder, http.StatusOK)
	upgraded := decodeBody[game.Result](t, recorder)
	if upgraded.State.Shops[shopIndex].Level != 2 || upgraded.State.Spent != slot.UnlockCost+a.config.UpgradeCosts[0] {
		t.Fatalf("升级结果不符：%+v", upgraded.State.Shops[shopIndex])
	}
	if upgraded.State.Shops[shopIndex].UpgradeCost == nil || *upgraded.State.Shops[shopIndex].UpgradeCost != a.config.UpgradeCosts[1] {
		t.Fatalf("升级后必须给出下一级成本：%+v", upgraded.State.Shops[shopIndex])
	}
	// 两套 ID 不得混用：slotId 传给升级接口、shopId 传给解锁接口都必须是未知目标。
	before, saves := a.service.Snapshot(), a.store.saveCount()
	recorder = a.call(http.MethodPost, "/api/v1/shops/"+slot.ID+"/upgrade", "{}")
	assertErrorCode(t, recorder, http.StatusNotFound, "SHOP_NOT_FOUND")
	recorder = a.call(http.MethodPost, "/api/v1/slots/"+slot.ShopID+"/unlock", "{}")
	assertErrorCode(t, recorder, http.StatusNotFound, "SHOP_NOT_FOUND")
	if !reflect.DeepEqual(a.service.Snapshot(), before) || a.store.saveCount() != saves {
		t.Fatal("混用 ID 的请求不得改变状态")
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestCommandBodyValidation 验证命令请求体类型、形态与大小限制。
func TestCommandBodyValidation(t *testing.T) {
	a := newTestAPI(t)
	cases := []struct {
		name   string
		body   string
		mutate func(*http.Request)
		status int
		code   string
	}{
		{"缺少请求类型", "{}", func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusUnsupportedMediaType, "JSON_REQUIRED"},
		{"错误请求类型", "{}", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType, "JSON_REQUIRED"},
		{"表单请求类型", "{}", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusUnsupportedMediaType, "JSON_REQUIRED"},
		{"允许字符集参数", "{}", func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }, http.StatusOK, ""},
		{"空请求体", "", nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"null请求体", "null", nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"顶层数组", "[]", nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"非空对象", `{"coins":5}`, nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"客户端时间", `{"now":"2026-09-13T00:00:00Z"}`, nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"客户端客流", `{"visitors":50}`, nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"两个JSON值", "{} {}", nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"尾随垃圾", "{} garbage", nil, http.StatusBadRequest, "INVALID_COMMAND"},
		{"请求体超限", `{"pad":"` + strings.Repeat("x", 2000) + `"}`, nil, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mutate == nil {
				tc.mutate = func(*http.Request) {}
			}
			recorder := a.call(http.MethodPost, "/api/v1/mall/settle", tc.body, tc.mutate)
			if tc.code == "" {
				assertStatus(t, recorder, tc.status)
				return
			}
			assertErrorCode(t, recorder, tc.status, tc.code)
		})
	}
	if a.store.saveCount() != 1 {
		t.Fatalf("非法请求体不得写入存档，实际保存 %d 次", a.store.saveCount())
	}
	// 所有命令路由共用同一套请求体校验，新增的两条命令不得例外。
	for _, route := range []string{"/api/v1/shops/coffee/prepare", "/api/v1/shops/coffee/upgrade", "/api/v1/slots/f1-s1/unlock"} {
		recorder := a.call(http.MethodPost, route, `{"coins":5}`)
		assertErrorCode(t, recorder, http.StatusBadRequest, "INVALID_COMMAND")
		recorder = a.call(http.MethodPost, route, "{}", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
		assertErrorCode(t, recorder, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
	}
	if a.store.saveCount() != 1 {
		t.Fatalf("非法请求体不得写入存档，实际保存 %d 次", a.store.saveCount())
	}
}

// brokenWriter 让响应写入失败，用于覆盖响应编码与写出的兜底分支。
type brokenWriter struct{ header http.Header }

func (w *brokenWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func (w *brokenWriter) WriteHeader(int) {}

// TestWriteJSONFallbacks 验证无法编码或无法写出时的兜底行为。
func TestWriteJSONFallbacks(t *testing.T) {
	// 无法编码：返回 500 且不写出任何 JSON 正文。
	recorder := httptest.NewRecorder()
	writeJSON(recorder, http.StatusOK, make(chan int))
	assertStatus(t, recorder, http.StatusInternalServerError)
	// 无法写出：不 panic，也不影响后续请求。
	writeJSON(&brokenWriter{}, http.StatusOK, game.Result{})
}

// TestErrorMapping 验证领域错误到稳定 HTTP 错误码的映射。
func TestErrorMapping(t *testing.T) {
	t.Run("时钟回退", func(t *testing.T) {
		a := newTestAPI(t)
		a.clock.set(testTime().Add(5 * time.Second))
		assertStatus(t, a.call(http.MethodPost, "/api/v1/mall/settle", "{}"), http.StatusOK)
		before, saves := a.service.Snapshot(), a.store.saveCount()
		a.clock.set(testTime().Add(4 * time.Second))
		recorder := a.call(http.MethodPost, "/api/v1/mall/settle", "{}")
		assertErrorCode(t, recorder, http.StatusConflict, "CLOCK_BACKWARDS")
		if !reflect.DeepEqual(a.service.Snapshot(), before) || a.store.saveCount() != saves {
			t.Fatal("时钟回退不得改变状态")
		}
	})
	t.Run("存档失败", func(t *testing.T) {
		a := newTestAPI(t)
		a.clock.set(testTime().Add(5 * time.Second))
		a.store.failSave(errors.New("注入存档写入失败"))
		recorder := a.call(http.MethodPost, "/api/v1/shops/coffee/prepare", "{}")
		assertErrorCode(t, recorder, http.StatusInternalServerError, "SAVE_FAILED")
		if strings.Contains(recorder.Body.String(), "注入存档写入失败") {
			t.Fatal("内部错误细节不得返回客户端")
		}
		if !strings.Contains(a.logs.String(), "operation failed") {
			t.Fatal("服务端必须记录操作失败日志")
		}
		if a.service.Snapshot().Revision != 1 {
			t.Fatal("保存失败不得发布部分结果")
		}
		a.store.failSave(nil)
		assertStatus(t, a.call(http.MethodPost, "/api/v1/shops/coffee/prepare", "{}"), http.StatusOK)
	})
	t.Run("数值上限", func(t *testing.T) {
		a := newTestAPI(t)
		a.store.seed(t, func(state *game.State) { state.Revision = testMaxSafeInteger })
		a.start()
		a.clock.set(testTime().Add(time.Second))
		recorder := a.call(http.MethodPost, "/api/v1/shops/coffee/prepare", "{}")
		assertErrorCode(t, recorder, http.StatusConflict, "NUMERIC_LIMIT")
	})
	t.Run("解锁与升级错误码", func(t *testing.T) {
		a := newFundedTestAPI(t)
		slots := a.config.Slots
		cases := []struct {
			name   string
			target string
			status int
			code   string
		}{
			{"跳序解锁", "/api/v1/slots/" + slots[3].ID + "/unlock", http.StatusConflict, "SLOT_ORDER"},
			{"未知铺位", "/api/v1/slots/f3-s1/unlock", http.StatusNotFound, "SHOP_NOT_FOUND"},
			{"未解锁铺位开店", "/api/v1/shops/" + slots[2].ShopID + "/prepare", http.StatusConflict, "SLOT_LOCKED"},
			{"未解锁铺位升级", "/api/v1/shops/" + slots[2].ShopID + "/upgrade", http.StatusConflict, "SLOT_LOCKED"},
			{"待开业店铺升级", "/api/v1/shops/flowers/upgrade", http.StatusConflict, "SHOP_NOT_OPEN"},
		}
		before, saves := a.service.Snapshot(), a.store.saveCount()
		for _, tc := range cases {
			recorder := a.call(http.MethodPost, tc.target, "{}")
			assertErrorCode(t, recorder, tc.status, tc.code)
		}
		if !reflect.DeepEqual(a.service.Snapshot(), before) || a.store.saveCount() != saves {
			t.Fatal("被拒绝的命令不得改变状态或写入存档")
		}
		// 满级：把第一家店升到顶。
		assertStatus(t, a.call(http.MethodPost, "/api/v1/shops/coffee/prepare", "{}"), http.StatusOK)
		levels := len(a.config.Shops[0].LevelCoinsPerVisitor)
		for level := 1; level < levels; level++ {
			assertStatus(t, a.call(http.MethodPost, "/api/v1/shops/coffee/upgrade", "{}"), http.StatusOK)
		}
		recorder := a.call(http.MethodPost, "/api/v1/shops/coffee/upgrade", "{}")
		assertErrorCode(t, recorder, http.StatusConflict, "MAX_LEVEL")
		if view := a.service.Snapshot(); view.Shops[0].UpgradeCost != nil {
			t.Fatalf("满级店铺的 upgradeCost 必须为 null：%+v", view.Shops[0])
		}
		// 金币不足：另起一份初始金币恰好只够第一个二层铺位的存档。
		poor := fundedConfig(t)
		poor.InitialCoins = slots[2].UnlockCost
		b := newTestAPIWithConfig(t, testDevToken, []string{testOrigin}, poor)
		assertStatus(t, b.call(http.MethodPost, "/api/v1/slots/"+slots[2].ID+"/unlock", "{}"), http.StatusOK)
		if state := b.service.Snapshot(); state.Coins >= slots[3].UnlockCost {
			t.Fatalf("测试前提不成立：余额 %d 仍买得起 %s", state.Coins, slots[3].ID)
		}
		assertErrorCode(t, b.call(http.MethodPost, "/api/v1/slots/"+slots[3].ID+"/unlock", "{}"), http.StatusConflict, "INSUFFICIENT_COINS")
	})
}

// TestRoutingErrors 验证未知路径与错误方法的响应。
func TestRoutingErrors(t *testing.T) {
	a := newTestAPI(t)
	cases := []struct {
		method string
		target string
		status int
	}{
		{http.MethodGet, "/api/v1/unknown", http.StatusNotFound},
		{http.MethodPost, "/api/v1/unknown", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/mall/settle", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/v1/mall", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/shops/coffee/prepare", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/shops/coffee/upgrade", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/slots/f2-s1/unlock", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v1/slots/unlock", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			assertStatus(t, a.call(tc.method, tc.target, ""), tc.status)
		})
	}
	if a.store.saveCount() != 1 {
		t.Fatal("无效路由不得写入存档")
	}
}

// TestRequestLogDoesNotLeakSecrets 验证访问日志不含令牌与查询参数。
func TestRequestLogDoesNotLeakSecrets(t *testing.T) {
	a := newTestAPI(t)
	recorder := a.call(http.MethodGet, "/api/v1/mall?"+leakSecret+"="+url.QueryEscape(leakSecret), "", func(r *http.Request) {
		r.Header.Set("X-Dev-Token", leakSecret)
	})
	assertStatus(t, recorder, http.StatusOK)
	logs := a.logs.String()
	if strings.Contains(logs, a.token) || strings.Contains(logs, leakSecret) {
		t.Fatalf("访问日志泄露敏感信息：%s", logs)
	}
	if !strings.Contains(logs, "local API request") || !strings.Contains(logs, `"/api/v1/mall"`) {
		t.Fatalf("访问日志缺少必要字段：%s", logs)
	}
}

// TestConcurrentPrepareThroughHandler 验证并发命令经由处理器仍只提交一次。
func TestConcurrentPrepareThroughHandler(t *testing.T) {
	a := newTestAPI(t)
	const workers = 50
	start := make(chan struct{})
	results := make(chan game.Result, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			recorder := httptest.NewRecorder()
			a.handler.ServeHTTP(recorder, a.newRequest(http.MethodPost, "/api/v1/shops/coffee/prepare", "{}"))
			if recorder.Code != http.StatusOK {
				results <- game.Result{}
				return
			}
			var result game.Result
			if json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
				results <- game.Result{}
				return
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)
	changed := 0
	for result := range results {
		if result.Changed {
			changed++
		}
		if result.EarnedCoins != 0 || result.VisitorsUsed != 0 {
			t.Fatal("同一时刻并发准备不应产出收益")
		}
	}
	state := a.service.Snapshot()
	if changed != 1 || a.store.saveCount() != 2 || state.Revision != 2 || !state.Shops[0].Prepared {
		t.Fatalf("并发准备应只生效一次：changed=%d，saves=%d，state=%+v", changed, a.store.saveCount(), state)
	}
}

// TestLoopbackHelpers 验证主机头与来源地址的判定边界。
func TestLoopbackHelpers(t *testing.T) {
	hosts := map[string]bool{
		"127.0.0.1:8080": true, "localhost:8080": true, "[::1]:8080": true, "127.0.0.1": true,
		// 主机名按字面精确匹配；浏览器与客户端均使用小写主机名。
		"LOCALHOST:8080": false, "mall.example.com:8080": false, "0.0.0.0:8080": false,
		"10.0.0.1:8080": false, "": false,
	}
	for host, want := range hosts {
		if got := loopbackHost(host); got != want {
			t.Fatalf("loopbackHost(%q) = %t，期望 %t", host, got, want)
		}
	}
	remotes := map[string]bool{
		"127.0.0.1:54321": true, "[::1]:54321": true,
		"203.0.113.9:51000": false, "127.0.0.1": false, "": false,
	}
	for remote, want := range remotes {
		if got := loopbackRemote(remote); got != want {
			t.Fatalf("loopbackRemote(%q) = %t，期望 %t", remote, got, want)
		}
	}
}
