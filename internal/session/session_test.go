package session

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubClock is a mutable clock for expiry tests.
type stubClock struct {
	mu    sync.Mutex
	stamp time.Time
}

func (c *stubClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stamp
}

func (c *stubClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stamp = c.stamp.Add(d)
}

func newTestStore(t *testing.T, ttl time.Duration) (*Store, *stubClock) {
	t.Helper()
	clock := &stubClock{stamp: time.Date(2026, time.September, 23, 4, 0, 0, 0, time.UTC)}
	store, err := NewStore(ttl, clock.now)
	if err != nil {
		t.Fatalf("创建会话存储失败：%v", err)
	}
	return store, clock
}

// TestIssueAndAuthenticate 验证签发与校验的基本契约。
func TestIssueAndAuthenticate(t *testing.T) {
	store, _ := newTestStore(t, 10*time.Minute)
	token, err := store.Issue("account-a")
	if err != nil {
		t.Fatalf("签发失败：%v", err)
	}
	if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
		t.Fatalf("令牌必须是 64 位小写十六进制：%q", token)
	}
	key, err := store.Authenticate(token)
	if err != nil || key != "account-a" {
		t.Fatalf("校验结果 = %q, %v，期望 account-a", key, err)
	}
	// 同一账号可并存多个令牌（多设备），且每次签发都是新熵。
	other, err := store.Issue("account-a")
	if err != nil || other == token {
		t.Fatalf("二次签发必须给出不同令牌：%q, %v", other, err)
	}
	if key, err = store.Authenticate(other); err != nil || key != "account-a" {
		t.Fatalf("第二个令牌也应有效：%q, %v", key, err)
	}
}

// TestNewStoreRejectsInvalidOptions 验证寿命与时钟必须显式合法。
func TestNewStoreRejectsInvalidOptions(t *testing.T) {
	clock := time.Date(2026, time.September, 23, 4, 0, 0, 0, time.UTC)
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := NewStore(ttl, func() time.Time { return clock }); err == nil {
			t.Fatalf("非正寿命 %v 必须被拒绝", ttl)
		}
	}
	if _, err := NewStore(time.Minute, nil); err == nil {
		t.Fatal("缺少时钟必须被拒绝")
	}
}

// TestIssueRejectsEmptyAccountKey 验证没有账号键就签不出票。
func TestIssueRejectsEmptyAccountKey(t *testing.T) {
	store, _ := newTestStore(t, time.Minute)
	if _, err := store.Issue(""); err == nil {
		t.Fatal("空账号键必须被拒绝")
	}
}

// TestAuthenticateRejectsUnknownTokens 验证未知与空令牌的区分。
func TestAuthenticateRejectsUnknownTokens(t *testing.T) {
	store, _ := newTestStore(t, 10*time.Minute)
	for _, token := range []string{"", strings.Repeat("a", 64), "not-hex-at-all"} {
		if _, err := store.Authenticate(token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Authenticate(%q) 错误 = %v，期望 ErrInvalidToken", token, err)
		}
	}
}

// TestAuthenticateReportsExpiry 验证过期令牌报 ErrExpiredToken，被清扫后转 ErrInvalidToken。
func TestAuthenticateReportsExpiry(t *testing.T) {
	store, clock := newTestStore(t, 10*time.Minute)
	token, err := store.Issue("account-a")
	if err != nil {
		t.Fatal(err)
	}
	// 差 1 秒到期：仍然有效。
	clock.advance(10*time.Minute - time.Second)
	if _, err = store.Authenticate(token); err != nil {
		t.Fatalf("未过期的令牌应有效：%v", err)
	}
	// 越过到期时刻：报过期（此时条目还在，只是失效）。
	clock.advance(2 * time.Second)
	if _, err = store.Authenticate(token); !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("过期令牌错误 = %v，期望 ErrExpiredToken", err)
	}
	// 再一次签发会清扫掉它：此后同一令牌按「不存在」处理。
	if _, err = store.Issue("account-b"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authenticate(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("被清扫的令牌错误 = %v，期望 ErrInvalidToken", err)
	}
}

// TestSweepReclaimsExpiredTokens 验证清扫真的回收条目，而不是只靠断言想象。
func TestSweepReclaimsExpiredTokens(t *testing.T) {
	store, clock := newTestStore(t, time.Minute)
	// 20s 间隔签发三次：最早到期的也比最后一次签发晚 20s，此前不会被误扫。
	for range 3 {
		if _, err := store.Issue("account-a"); err != nil {
			t.Fatal(err)
		}
		clock.advance(20 * time.Second)
	}
	if got := store.liveTokens(); got != 3 {
		t.Fatalf("到期前应有 3 个令牌，实际 %d", got)
	}
	// 越过全部到期时刻后再签发：旧的被清扫，只剩新的这一个。
	clock.advance(2 * time.Minute)
	if _, err := store.Issue("account-b"); err != nil {
		t.Fatal(err)
	}
	if got := store.liveTokens(); got != 1 {
		t.Fatalf("清扫后应只剩 1 个令牌，实际 %d", got)
	}
}

// TestConcurrentIssueAndAuthenticate 验证并发签发与校验不互坏（竞态检测在 CI 的 -race 下生效）。
func TestConcurrentIssueAndAuthenticate(t *testing.T) {
	store, clock := newTestStore(t, time.Minute)
	const workers = 40
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			token, err := store.Issue("account-a")
			if err != nil {
				t.Errorf("并发签发失败：%v", err)
				return
			}
			if _, err := store.Authenticate(token); err != nil && worker%2 == 0 {
				// 时钟不动，签发的令牌必然还在寿命内。
				t.Errorf("并发校验失败：%v", err)
			}
		}(i)
	}
	close(start)
	group.Wait()
	// 全部越过到期时刻后再签发一次：清扫应把它们全部回收，只剩新的这一个。
	clock.advance(2 * time.Minute)
	if _, err := store.Issue("account-b"); err != nil {
		t.Fatal(err)
	}
	if got := store.liveTokens(); got != 1 {
		t.Fatalf("清扫后应只剩 1 个令牌，实际 %d", got)
	}
}

// TestAccountKey 验证账号键的形态：定长十六进制、确定、不含 openid 原文。
func TestAccountKey(t *testing.T) {
	first := AccountKey("oXyz123")
	if len(first) != 32 || strings.Trim(first, "0123456789abcdef") != "" {
		t.Fatalf("账号键必须是 32 位小写十六进制：%q", first)
	}
	if first != AccountKey("oXyz123") {
		t.Fatal("同一 openid 必须得到同一账号键")
	}
	if first == AccountKey("oXyz124") {
		t.Fatal("不同 openid 必须得到不同账号键")
	}
	if strings.Contains(first, "oXyz123") {
		t.Fatal("账号键不得包含 openid 原文")
	}
}

// TestValidateAccountKey 验证账号键的文件名安全判定。
func TestValidateAccountKey(t *testing.T) {
	valid := []string{"dev-alice", AccountKey("oXyz123"), "a"}
	for _, key := range valid {
		if !ValidateAccountKey(key) {
			t.Fatalf("%q 应判为安全", key)
		}
	}
	invalid := []string{"", "../dev-alice", "a/b", `a\b`, strings.Repeat("a", 65)}
	for _, key := range invalid {
		if ValidateAccountKey(key) {
			t.Fatalf("%q 应判为不安全", key)
		}
	}
}
