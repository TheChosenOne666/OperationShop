package game

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu    sync.Mutex
	stamp time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stamp
}

func (c *testClock) set(stamp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stamp = stamp
}

type memoryStore struct {
	mu      sync.Mutex
	state   State
	exists  bool
	loadErr error
	saveErr error
	loads   int
	saves   int
}

// Load 返回内存存档的副本，或注入的读取错误。
func (m *memoryStore) Load() (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	if m.loadErr != nil {
		return State{}, m.loadErr
	}
	if !m.exists {
		return State{}, os.ErrNotExist
	}
	return copyTestState(m.state), nil
}

// Save 保存输入的副本；注入错误时保留原有存档。
func (m *memoryStore) Save(state State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.state, m.exists = copyTestState(state), true
	return nil
}

func (m *memoryStore) failSave(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveErr = err
}

func (m *memoryStore) saveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves
}

func copyTestState(state State) State {
	state.Shops = append([]ShopState(nil), state.Shops...)
	return state
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadConfig(filepath.Join("..", "..", "config", "development.json"))
	if err != nil {
		t.Fatalf("读取开发配置失败：%v", err)
	}
	return cfg
}

func testTime() time.Time {
	return time.Date(2026, time.September, 12, 4, 0, 0, 0, time.UTC)
}

func newTestService(t *testing.T) (*Service, *memoryStore, *testClock) {
	t.Helper()
	store := &memoryStore{}
	clock := &testClock{stamp: testTime()}
	service, err := NewService(testConfig(t), store, clock.now)
	if err != nil {
		t.Fatalf("创建本地开发服务失败：%v", err)
	}
	return service, store, clock
}

func prepareTestShops(t *testing.T, service *Service, ids ...string) {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"clothing", "dessert", "bookstore", "coffee", "flowers"}
	}
	for _, id := range ids {
		result, err := service.Prepare(id)
		if err != nil {
			t.Fatalf("准备店铺 %q 失败：%v", id, err)
		}
		assertTestResult(t, result, 0, 0, true)
	}
}

func settleTestService(t *testing.T, service *Service) Result {
	t.Helper()
	result, err := service.Settle()
	if err != nil {
		t.Fatalf("结算失败：%v", err)
	}
	return result
}

func assertTestResult(t *testing.T, got Result, coins, visitors int64, changed bool) {
	t.Helper()
	if got.EarnedCoins != coins || got.VisitorsUsed != visitors || got.Changed != changed {
		t.Fatalf("结算结果 = (%d 币, %d 客流, changed=%t)，期望 (%d, %d, %t)",
			got.EarnedCoins, got.VisitorsUsed, got.Changed, coins, visitors, changed)
	}
}

func assertTestState(t *testing.T, got, want State) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("状态不符\n实际：%+v\n期望：%+v", got, want)
	}
}

func assertTestStoredState(t *testing.T, store Store, want State) {
	t.Helper()
	got, err := store.Load()
	if err != nil {
		t.Fatalf("读取已提交状态失败：%v", err)
	}
	assertTestState(t, got, want)
}

// TestServiceInitialState 验证本地开发账号的初始资源与未准备店铺。
func TestServiceInitialState(t *testing.T) {
	service, store, _ := newTestService(t)
	want := State{
		SchemaVersion: 1, RulesFingerprint: fingerprint(testConfig(t)), Revision: 1,
		Coins: 1280, BusinessDay: "2026-09-12", VisitorsRemaining: 50,
		LastAccrualAt: testTime(), LastObservedAt: testTime(),
		Shops: []ShopState{{ID: "clothing"}, {ID: "dessert"}, {ID: "bookstore"}, {ID: "coffee"}, {ID: "flowers"}},
	}
	assertTestState(t, service.Snapshot(), want)
	assertTestStoredState(t, store, want)
	if store.saveCount() != 1 {
		t.Fatalf("首次初始化应仅保存一次，实际 %d 次", store.saveCount())
	}
	assertTestResult(t, settleTestService(t, service), 0, 0, false)
	if store.saveCount() != 1 {
		t.Fatal("相同时刻重复结算不应保存")
	}
}

// TestServiceUnpreparedShopsDoNotAccrue 验证未准备期间不产出且不能追补。
func TestServiceUnpreparedShopsDoNotAccrue(t *testing.T) {
	service, store, clock := newTestService(t)
	clock.set(testTime().Add(31*time.Second + 200*time.Millisecond))
	result := settleTestService(t, service)
	assertTestResult(t, result, 0, 0, true)
	if result.State.Coins != 1280 || result.State.VisitorsRemaining != 50 || !result.State.LastAccrualAt.Equal(clock.now()) {
		t.Fatalf("未准备期间不应积累收益或保留可追补时间：%+v", result.State)
	}
	for _, shop := range result.State.Shops {
		if shop.Prepared || shop.Visitors != 0 || shop.Revenue != 0 {
			t.Fatalf("未准备店铺发生变化：%+v", shop)
		}
	}
	clock.set(testTime().Add(time.Minute))
	prepareTestShops(t, service, "clothing")
	if !service.Snapshot().LastAccrualAt.Equal(clock.now()) {
		t.Fatal("首次准备必须以准备时刻开始计时")
	}
	clock.set(testTime().Add(time.Minute + 5*time.Second - time.Nanosecond))
	assertTestResult(t, settleTestService(t, service), 0, 0, true)
	clock.set(testTime().Add(time.Minute + 5*time.Second))
	assertTestResult(t, settleTestService(t, service), 12, 1, true)
	assertTestStoredState(t, store, service.Snapshot())
}

// TestServicePrepareIsIdempotent 验证重复准备不结算、不保存且不改变状态。
func TestServicePrepareIsIdempotent(t *testing.T) {
	service, store, clock := newTestService(t)
	prepareTestShops(t, service, "clothing")
	before, saves := service.Snapshot(), store.saveCount()
	for _, stamp := range []time.Time{testTime(), testTime().Add(25 * time.Second)} {
		clock.set(stamp)
		result, err := service.Prepare("clothing")
		if err != nil {
			t.Fatal(err)
		}
		assertTestResult(t, result, 0, 0, false)
		assertTestState(t, result.State, before)
		assertTestState(t, service.Snapshot(), before)
		if store.saveCount() != saves {
			t.Fatal("重复准备不应写入存档")
		}
	}
	assertTestResult(t, settleTestService(t, service), 60, 5, true)
}

// TestServicePrepareSettlesExistingShopsFirst 验证新增准备店铺不分享准备前的收益。
func TestServicePrepareSettlesExistingShopsFirst(t *testing.T) {
	service, _, clock := newTestService(t)
	prepareTestShops(t, service, "clothing")
	clock.set(testTime().Add(10 * time.Second))
	result, err := service.Prepare("dessert")
	if err != nil {
		t.Fatal(err)
	}
	assertTestResult(t, result, 24, 2, true)
	if result.State.Shops[0].Visitors != 2 || result.State.Shops[1].Visitors != 0 || !result.State.Shops[1].Prepared {
		t.Fatalf("准备前的收益只能归已准备店铺：%+v", result.State.Shops)
	}
	clock.set(testTime().Add(15 * time.Second))
	result = settleTestService(t, service)
	assertTestResult(t, result, 8, 1, true)
	if result.State.Shops[1].Visitors != 1 {
		t.Fatal("新准备店铺应参与下一次轮询")
	}
}

// TestServiceSettlementRetainsRemainder 验证服务器五秒间隔及跨请求残余时间。
func TestServiceSettlementRetainsRemainder(t *testing.T) {
	service, store, clock := newTestService(t)
	prepareTestShops(t, service, "coffee")
	steps := []struct {
		elapsed  time.Duration
		accrued  time.Duration
		visitors int64
		total    int64
		changed  bool
	}{
		{5*time.Second - time.Nanosecond, 0, 0, 0, true},
		{5 * time.Second, 5 * time.Second, 1, 1, true},
		{12500 * time.Millisecond, 10 * time.Second, 1, 2, true},
		{15*time.Second - time.Nanosecond, 10 * time.Second, 0, 2, true},
		{15 * time.Second, 15 * time.Second, 1, 3, true},
		{15 * time.Second, 15 * time.Second, 0, 3, false},
	}
	for _, step := range steps {
		clock.set(testTime().Add(step.elapsed))
		saves := store.saveCount()
		result := settleTestService(t, service)
		assertTestResult(t, result, step.visitors*6, step.visitors, step.changed)
		if !result.State.LastAccrualAt.Equal(testTime().Add(step.accrued)) || !result.State.LastObservedAt.Equal(clock.now()) {
			t.Fatalf("经过 %s 后结算时间或观察时间不符：%+v", step.elapsed, result.State)
		}
		if result.State.Coins != 1280+step.total*6 || result.State.VisitorsRemaining != 50-step.total {
			t.Fatalf("经过 %s 后累计资源不符：%+v", step.elapsed, result.State)
		}
		wantSaves := saves
		if step.changed {
			wantSaves++
		}
		if store.saveCount() != wantSaves {
			t.Fatalf("保存次数 = %d，期望 %d", store.saveCount(), wantSaves)
		}
		assertTestStoredState(t, store, result.State)
	}
}

// TestServiceRoundRobin 验证五店配置顺序及每轮精确四十五币、五客流。
func TestServiceRoundRobin(t *testing.T) {
	service, _, clock := newTestService(t)
	prepareTestShops(t, service)
	prices := []int64{12, 8, 10, 6, 9}
	var coins, visitors int64
	for step := 0; step < 10; step++ {
		index := step % 5
		clock.set(testTime().Add(time.Duration(step+1) * 5 * time.Second))
		result := settleTestService(t, service)
		assertTestResult(t, result, prices[index], 1, true)
		coins += result.EarnedCoins
		visitors += result.VisitorsUsed
		if result.State.NextShopIndex != (index+1)%5 {
			t.Fatalf("第 %d 次轮询游标错误：%d", step+1, result.State.NextShopIndex)
		}
		for i, shop := range result.State.Shops {
			wantVisitors := int64((step + 1) / 5)
			if i < (step+1)%5 {
				wantVisitors++
			}
			if shop.Visitors != wantVisitors || shop.Revenue != wantVisitors*prices[i] {
				t.Fatalf("第 %d 次轮询店铺累计账目错误：%+v", step+1, shop)
			}
		}
		if step == 4 && (coins != 45 || visitors != 5 || result.State.Coins != 1325 || result.State.VisitorsRemaining != 45) {
			t.Fatalf("首轮必须精确产出 45 币并消耗 5 客流：%+v", result.State)
		}
	}
	if coins != 90 || visitors != 10 {
		t.Fatalf("两轮收益 = %d/%d，期望 90/10", coins, visitors)
	}
}

// TestServiceRoundRobinSkipsUnprepared 验证轮询跳过未准备店铺并循环回绕。
func TestServiceRoundRobinSkipsUnprepared(t *testing.T) {
	service, _, clock := newTestService(t)
	prepareTestShops(t, service, "dessert", "flowers")
	for step, index := range []int{1, 4, 1, 4} {
		before := service.Snapshot()
		clock.set(testTime().Add(time.Duration(step+1) * 5 * time.Second))
		result := settleTestService(t, service)
		assertTestResult(t, result, []int64{12, 8, 10, 6, 9}[index], 1, true)
		for i, shop := range result.State.Shops {
			if i == index {
				if shop.Visitors != before.Shops[i].Visitors+1 {
					t.Fatalf("预期轮到店铺 %s：%+v", shop.ID, result.State.Shops)
				}
			} else if shop != before.Shops[i] {
				t.Fatalf("非轮询店铺发生变化：%+v", shop)
			}
		}
	}
}

// TestServiceVisitorsExhausted 验证单日五十客流上限与耗尽后不再产出。
func TestServiceVisitorsExhausted(t *testing.T) {
	service, _, clock := newTestService(t)
	prepareTestShops(t, service)
	clock.set(testTime().Add(1001 * time.Second))
	result := settleTestService(t, service)
	assertTestResult(t, result, 450, 50, true)
	if result.State.Coins != 1730 || result.State.VisitorsRemaining != 0 || result.State.NextShopIndex != 0 {
		t.Fatalf("耗尽状态错误：%+v", result.State)
	}
	for i, shop := range result.State.Shops {
		if shop.Visitors != 10 || shop.Revenue != 10*[]int64{12, 8, 10, 6, 9}[i] {
			t.Fatalf("耗尽时各店铺应各接待十客：%+v", shop)
		}
	}
	before := result.State
	clock.set(testTime().Add(time.Hour))
	result = settleTestService(t, service)
	assertTestResult(t, result, 0, 0, true)
	if result.State.Coins != before.Coins || result.State.VisitorsRemaining != 0 || !reflect.DeepEqual(result.State.Shops, before.Shops) {
		t.Fatal("客流耗尽后不能继续产出")
	}
	if !result.State.LastAccrualAt.Equal(clock.now()) {
		t.Fatal("耗尽期间不应保留待补时间")
	}
}

// TestServiceReconnectSameDay 验证同日重连延续存档游标及未满五秒的残余。
func TestServiceReconnectSameDay(t *testing.T) {
	service, store, clock := newTestService(t)
	prepareTestShops(t, service)
	clock.set(testTime().Add(12 * time.Second))
	assertTestResult(t, settleTestService(t, service), 20, 2, true)
	before, saves := service.Snapshot(), store.saveCount()
	clock.set(testTime().Add(19 * time.Second))
	reconnected, err := NewService(testConfig(t), store, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	assertTestState(t, reconnected.Snapshot(), before)
	if store.saveCount() != saves {
		t.Fatal("加载已有存档不应自动结算或覆盖")
	}
	result := settleTestService(t, reconnected)
	assertTestResult(t, result, 10, 1, true)
	if !result.State.LastAccrualAt.Equal(testTime().Add(15 * time.Second)) {
		t.Fatal("重连必须保留原有结算间隔的残余时间")
	}
	clock.set(testTime().Add(20 * time.Second))
	result = settleTestService(t, reconnected)
	assertTestResult(t, result, 6, 1, true)
	if result.State.Coins != 1316 || result.State.VisitorsRemaining != 46 || result.State.NextShopIndex != 4 {
		t.Fatalf("重连结算资源或轮询顺序错误：%+v", result.State)
	}
}

// TestServiceShanghaiDayRefresh 验证上海日期刷新、不补历史天并保留累计账目。
func TestServiceShanghaiDayRefresh(t *testing.T) {
	start := time.Date(2026, time.September, 12, 15, 59, 50, 0, time.UTC)
	cases := []struct {
		name     string
		days     int
		offset   time.Duration
		restart  bool
		visitors int64
		coins    int64
	}{
		{"次日零点立即刷新", 0, 0, false, 0, 0},
		{"上海跨日但UTC尚未跨日", 0, 7500 * time.Millisecond, false, 1, 8},
		{"离线多天只结算今天", 4, 7500 * time.Millisecond, true, 1, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{stamp: start}
			store := &memoryStore{}
			service, err := NewService(testConfig(t), store, clock.now)
			if err != nil {
				t.Fatal(err)
			}
			prepareTestShops(t, service)
			clock.set(start.Add(5 * time.Second))
			assertTestResult(t, settleTestService(t, service), 12, 1, true)
			before := service.Snapshot()
			midnight := start.Add(10*time.Second + time.Duration(tc.days)*24*time.Hour)
			clock.set(midnight.Add(tc.offset))
			if tc.restart {
				service, err = NewService(testConfig(t), store, clock.now)
				if err != nil {
					t.Fatal(err)
				}
			}
			assertTestState(t, service.Snapshot(), before)
			result := settleTestService(t, service)
			assertTestResult(t, result, tc.coins, tc.visitors, true)
			wantDay := time.Date(2026, time.September, 13+tc.days, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
			if result.State.BusinessDay != wantDay || result.State.VisitorsRemaining != 50-tc.visitors || result.State.Coins != 1292+tc.coins {
				t.Fatalf("刷新后仅应补充当天客流及当天收益：%+v", result.State)
			}
			if !result.State.LastAccrualAt.Equal(midnight.Add(time.Duration(tc.visitors)*5*time.Second)) || result.State.NextShopIndex != 1+int(tc.visitors) {
				t.Fatalf("跨日后时间残余或轮询游标错误：%+v", result.State)
			}
			wantShops := append([]ShopState(nil), before.Shops...)
			wantShops[1].Visitors += tc.visitors
			wantShops[1].Revenue += tc.coins
			if !reflect.DeepEqual(result.State.Shops, wantShops) {
				t.Fatalf("跨日不应清空准备状态或累计账目：%+v", result.State.Shops)
			}
			saves := store.saveCount()
			assertTestResult(t, settleTestService(t, service), 0, 0, false)
			if store.saveCount() != saves {
				t.Fatal("同一时刻重复请求不能再次刷新客流")
			}
			clock.set(midnight.Add(10 * time.Second))
			result = settleTestService(t, service)
			assertTestResult(t, result, 18-tc.coins, 2-tc.visitors, true)
			if result.State.Coins != 1310 || result.State.VisitorsRemaining != 48 {
				t.Fatalf("刷新后残余时间未正确延续：%+v", result.State)
			}
		})
	}
}

// TestServiceUTCMidnightDoesNotRefresh 验证 UTC 零点不误刷新上海同日客流。
func TestServiceUTCMidnightDoesNotRefresh(t *testing.T) {
	clock := &testClock{stamp: time.Date(2026, time.September, 12, 23, 59, 55, 0, time.UTC)}
	service, err := NewService(testConfig(t), &memoryStore{}, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	prepareTestShops(t, service)
	clock.set(clock.now().Add(5 * time.Second))
	assertTestResult(t, settleTestService(t, service), 12, 1, true)
	clock.set(clock.now().Add(5 * time.Second))
	result := settleTestService(t, service)
	assertTestResult(t, result, 8, 1, true)
	if result.State.BusinessDay != "2026-09-13" || result.State.VisitorsRemaining != 48 {
		t.Fatalf("UTC 跨日不应刷新上海同日客流：%+v", result.State)
	}
}

// TestServiceRejectsClockBackwards 验证结算及首次准备拒绝服务器时钟回退。
func TestServiceRejectsClockBackwards(t *testing.T) {
	for _, operation := range []string{"结算", "准备"} {
		for _, rollback := range []time.Duration{time.Nanosecond, 24 * time.Hour} {
			t.Run(operation+"/"+rollback.String(), func(t *testing.T) {
				service, store, clock := newTestService(t)
				prepareTestShops(t, service, "clothing")
				clock.set(testTime().Add(12 * time.Second))
				settleTestService(t, service)
				before, saves := service.Snapshot(), store.saveCount()
				clock.set(before.LastObservedAt.Add(-rollback))
				var result Result
				var err error
				if operation == "准备" {
					result, err = service.Prepare("dessert")
				} else {
					result, err = service.Settle()
				}
				if !errors.Is(err, ErrClockBackwards) {
					t.Fatalf("期望时钟回退错误，实际 %v", err)
				}
				if !reflect.DeepEqual(result, Result{}) {
					t.Fatalf("失败操作不能返回已提交结果：%+v", result)
				}
				assertTestState(t, service.Snapshot(), before)
				assertTestStoredState(t, store, before)
				if store.saveCount() != saves {
					t.Fatal("时钟回退不能保存状态")
				}
				clock.set(before.LastObservedAt.Add(3 * time.Second))
				assertTestResult(t, settleTestService(t, service), 12, 1, true)
			})
		}
	}
}

// TestServiceUnknownShopDoesNotMutate 验证未知店铺不触发结算或日期刷新。
func TestServiceUnknownShopDoesNotMutate(t *testing.T) {
	for _, elapsed := range []time.Duration{25 * time.Second, 48 * time.Hour} {
		t.Run(elapsed.String(), func(t *testing.T) {
			service, store, clock := newTestService(t)
			prepareTestShops(t, service)
			before, saves := service.Snapshot(), store.saveCount()
			clock.set(testTime().Add(elapsed))
			for _, id := range []string{"", "unknown", "Clothing", " clothing "} {
				result, err := service.Prepare(id)
				if !errors.Is(err, ErrUnknownShop) || !reflect.DeepEqual(result, Result{}) {
					t.Fatalf("未知店铺 %q 返回结果错误：%+v，%v", id, result, err)
				}
				assertTestState(t, service.Snapshot(), before)
				assertTestStoredState(t, store, before)
				if store.saveCount() != saves {
					t.Fatal("未知店铺请求不能保存状态")
				}
			}
		})
	}
}

// TestServiceSaveFailureRollsBack 验证保存失败时资源、准备状态和时间全部回滚。
func TestServiceSaveFailureRollsBack(t *testing.T) {
	cases := []struct {
		name     string
		elapsed  time.Duration
		prepare  string
		coins    int64
		visitors int64
	}{
		{"首次准备失败", 0, "dessert", 0, 0},
		{"结算并准备失败", 10 * time.Second, "dessert", 24, 2},
		{"普通结算失败", 12 * time.Second, "", 24, 2},
		{"跨日刷新失败", 12*time.Hour + 10*time.Second, "", 24, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, store, clock := newTestService(t)
			prepareTestShops(t, service, "clothing")
			before, saves := service.Snapshot(), store.saveCount()
			clock.set(testTime().Add(tc.elapsed))
			injected := errors.New("注入保存失败")
			store.failSave(injected)
			operation := service.Settle
			if tc.prepare != "" {
				operation = func() (Result, error) { return service.Prepare(tc.prepare) }
			}
			result, err := operation()
			if !errors.Is(err, injected) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("保存错误必须传回且不能返回部分收益：%+v，%v", result, err)
			}
			assertTestState(t, service.Snapshot(), before)
			assertTestStoredState(t, store, before)
			if store.saveCount() != saves+1 {
				t.Fatal("失败操作应只尝试一次保存")
			}
			store.failSave(nil)
			result, err = operation()
			if err != nil {
				t.Fatal(err)
			}
			assertTestResult(t, result, tc.coins, tc.visitors, true)
			if result.State.Revision != before.Revision+1 || result.State.Coins != before.Coins+tc.coins || result.State.VisitorsRemaining != 50-tc.visitors {
				t.Fatalf("失败后重试不能丢失或重复收益：%+v", result.State)
			}
			if tc.prepare != "" && !result.State.Shops[1].Prepared {
				t.Fatal("重试成功应完成准备")
			}
			assertTestStoredState(t, store, result.State)
			assertTestResult(t, settleTestService(t, service), 0, 0, false)
		})
	}
}

// TestServiceCopiesAreIsolated 验证配置、快照及操作结果与内部切片隔离。
func TestServiceCopiesAreIsolated(t *testing.T) {
	cfg := testConfig(t)
	clock := &testClock{stamp: testTime()}
	store := &memoryStore{}
	service, err := NewService(cfg, store, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := service.Configuration()
	wantState := service.Snapshot()
	cfg.Shops[0].CoinsPerVisitor = 999
	cfg.Shops[0].ID = "mutated-input"
	cfg.InitialCoins = 0
	returned := service.Configuration()
	returned.Shops[1].Name = "修改副本"
	returned.Shops[1].CoinsPerVisitor = 999
	returned.DailyVisitors = 1
	if !reflect.DeepEqual(service.Configuration(), wantConfig) {
		t.Fatal("输入配置或返回配置的修改污染了内部规则")
	}
	snapshot := service.Snapshot()
	snapshot.Coins = 0
	snapshot.Shops[0].Prepared = true
	snapshot.Shops[0].ID = "mutated-snapshot"
	clock.set(testTime().Add(24 * time.Hour))
	assertTestState(t, service.Snapshot(), wantState)
	clock.set(testTime())
	prepared, err := service.Prepare("clothing")
	if err != nil {
		t.Fatal(err)
	}
	wantState = service.Snapshot()
	prepared.State.Shops[0].Prepared = false
	prepared.State.Coins = 0
	assertTestState(t, service.Snapshot(), wantState)
	clock.set(testTime().Add(5 * time.Second))
	settled := settleTestService(t, service)
	assertTestResult(t, settled, 12, 1, true)
	wantState = service.Snapshot()
	settled.State.Shops[0].Revenue = -1
	settled.State.Shops[0].Visitors = -1
	assertTestState(t, service.Snapshot(), wantState)
	assertTestStoredState(t, store, wantState)
	for _, operation := range []func() (Result, error){service.Settle, func() (Result, error) { return service.Prepare("clothing") }} {
		result, err := operation()
		if err != nil {
			t.Fatal(err)
		}
		assertTestResult(t, result, 0, 0, false)
		result.State.Shops[0].ID = "mutated-noop"
		assertTestState(t, service.Snapshot(), wantState)
	}
}

func concurrentTestOperations(t *testing.T, count int, operation func(int) (Result, error)) []Result {
	t.Helper()
	type outcome struct {
		result Result
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, count)
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			result, err := operation(index)
			outcomes <- outcome{result, err}
		}(i)
	}
	close(start)
	workers.Wait()
	close(outcomes)
	results := make([]Result, 0, count)
	for item := range outcomes {
		if item.err != nil {
			t.Fatalf("并发操作失败：%v", item.err)
		}
		results = append(results, item.result)
	}
	return results
}

// TestServiceConcurrentPrepareAndSettle 验证并发准备幂等且同一时间段只结算一次。
func TestServiceConcurrentPrepareAndSettle(t *testing.T) {
	service, store, clock := newTestService(t)
	ids := []string{"clothing", "dessert", "bookstore", "coffee", "flowers"}
	results := concurrentTestOperations(t, 100, func(index int) (Result, error) {
		return service.Prepare(ids[index%len(ids)])
	})
	changed := 0
	for _, result := range results {
		if result.Changed {
			changed++
		}
		if result.VisitorsUsed != 0 || result.EarnedCoins != 0 {
			t.Fatal("同一时刻并发准备不应产出")
		}
	}
	if changed != 5 || store.saveCount() != 6 || service.Snapshot().Revision != 6 {
		t.Fatalf("每个店铺只能准备一次：changed=%d，saves=%d", changed, store.saveCount())
	}
	clock.set(testTime().Add(25 * time.Second))
	results = concurrentTestOperations(t, 100, func(index int) (Result, error) {
		if index%2 == 0 {
			return service.Prepare(ids[(index/2)%len(ids)])
		}
		return service.Settle()
	})
	changed = 0
	var coins, visitors int64
	for _, result := range results {
		if result.Changed {
			changed++
		}
		coins += result.EarnedCoins
		visitors += result.VisitorsUsed
	}
	state := service.Snapshot()
	if changed != 1 || coins != 45 || visitors != 5 || store.saveCount() != 7 || state.Revision != 7 || state.Coins != 1325 || state.VisitorsRemaining != 45 {
		t.Fatalf("并发重复结算：changed=%d，coins=%d，visitors=%d，saves=%d，state=%+v", changed, coins, visitors, store.saveCount(), state)
	}
	for i, shop := range state.Shops {
		if !shop.Prepared || shop.Visitors != 1 || shop.Revenue != []int64{12, 8, 10, 6, 9}[i] {
			t.Fatalf("并发结算后店铺账目错误：%+v", shop)
		}
	}
	assertTestStoredState(t, store, state)
}

// TestServiceConcurrentNewPreparationAndSettle 验证首次准备与结算竞争时不追补新店收益。
func TestServiceConcurrentNewPreparationAndSettle(t *testing.T) {
	service, store, clock := newTestService(t)
	prepareTestShops(t, service, "clothing")
	clock.set(testTime().Add(25 * time.Second))
	results := concurrentTestOperations(t, 100, func(index int) (Result, error) {
		if index%2 == 0 {
			return service.Prepare("dessert")
		}
		return service.Settle()
	})
	var coins, visitors int64
	changed := 0
	for _, result := range results {
		coins += result.EarnedCoins
		visitors += result.VisitorsUsed
		if result.Changed {
			changed++
		}
	}
	state := service.Snapshot()
	if coins != 60 || visitors != 5 || state.Coins != 1340 || state.VisitorsRemaining != 45 {
		t.Fatalf("首次准备与结算竞争导致重复产出：coins=%d，visitors=%d，state=%+v", coins, visitors, state)
	}
	if (changed != 1 && changed != 2) || state.Revision != int64(2+changed) || store.saveCount() != 2+changed {
		t.Fatalf("准备与结算只能各提交至多一次：changed=%d，revision=%d，saves=%d", changed, state.Revision, store.saveCount())
	}
	if !state.Shops[1].Prepared || state.Shops[0].Visitors != 5 || state.Shops[1].Visitors != 0 || state.Shops[1].Revenue != 0 {
		t.Fatalf("并发首次准备的新店不能获得历史收益：%+v", state.Shops)
	}
	assertTestStoredState(t, store, state)
	clock.set(testTime().Add(30 * time.Second))
	assertTestResult(t, settleTestService(t, service), 8, 1, true)
}

// TestServiceExhaustedVisitorsRefreshNextDay 验证客流耗尽后在次日恢复并保留累计收益。
func TestServiceExhaustedVisitorsRefreshNextDay(t *testing.T) {
	service, store, clock := newTestService(t)
	prepareTestShops(t, service)
	clock.set(testTime().Add(time.Hour))
	assertTestResult(t, settleTestService(t, service), 450, 50, true)
	clock.set(testTime().Add(12*time.Hour + 10*time.Second))
	result := settleTestService(t, service)
	assertTestResult(t, result, 20, 2, true)
	if result.State.Coins != 1750 || result.State.VisitorsRemaining != 48 || result.State.BusinessDay != "2026-09-13" || result.State.NextShopIndex != 2 {
		t.Fatalf("耗尽后应补充次日客流：%+v", result.State)
	}
	for i, shop := range result.State.Shops {
		wantVisitors := int64(10)
		if i < 2 {
			wantVisitors++
		}
		if shop.Visitors != wantVisitors || shop.Revenue != wantVisitors*[]int64{12, 8, 10, 6, 9}[i] {
			t.Fatalf("刷新不能清空累计账目：%+v", shop)
		}
	}
	assertTestStoredState(t, store, result.State)
}

// TestServiceUnpreparedAcrossDays 验证跨日后首次准备不追补当天或历史未营业收益。
func TestServiceUnpreparedAcrossDays(t *testing.T) {
	service, _, clock := newTestService(t)
	clock.set(testTime().Add(72 * time.Hour))
	prepareTestShops(t, service, "clothing")
	state := service.Snapshot()
	if state.BusinessDay != "2026-09-15" || state.Coins != 1280 || state.VisitorsRemaining != 50 || !state.LastAccrualAt.Equal(clock.now()) {
		t.Fatalf("跨日未准备期间不能追补收益：%+v", state)
	}
	clock.set(clock.now().Add(5 * time.Second))
	assertTestResult(t, settleTestService(t, service), 12, 1, true)
}

// TestNewServiceDependenciesAndStoreErrors 验证构造参数及读写错误不被静默重置。
func TestNewServiceDependenciesAndStoreErrors(t *testing.T) {
	injected := errors.New("注入存储错误")
	cases := []struct {
		name      string
		store     *memoryStore
		nilStore  bool
		nilClock  bool
		badConfig bool
		wantError error
		wantLoads int
		wantSaves int
	}{
		{name: "缺少存储", nilStore: true},
		{name: "缺少时钟", store: &memoryStore{}, nilClock: true},
		{name: "非法配置先于存储访问", store: &memoryStore{}, badConfig: true},
		{name: "读取失败不创建新档", store: &memoryStore{loadErr: injected}, wantError: injected, wantLoads: 1},
		{name: "首次保存失败", store: &memoryStore{saveErr: injected}, wantError: injected, wantLoads: 1, wantSaves: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			if tc.badConfig {
				cfg.DailyVisitors = 0
			}
			var store Store = tc.store
			if tc.nilStore {
				store = nil
			}
			clock := testTime
			if tc.nilClock {
				clock = nil
			}
			service, err := NewService(cfg, store, clock)
			if service != nil || err == nil || (tc.wantError != nil && !errors.Is(err, tc.wantError)) {
				t.Fatalf("构造失败结果不符：service=%v，err=%v", service, err)
			}
			if tc.store != nil && (tc.store.loads != tc.wantLoads || tc.store.saveCount() != tc.wantSaves || tc.store.exists) {
				t.Fatalf("错误路径不应创建存档：loads=%d，saves=%d，exists=%t", tc.store.loads, tc.store.saveCount(), tc.store.exists)
			}
		})
	}
}

// TestNewServiceRejectsInvalidSaves 验证存档版本、计数、时间及累计账目校验。
func TestNewServiceRejectsInvalidSaves(t *testing.T) {
	service, _, _ := newTestService(t)
	base := service.Snapshot()
	cases := []struct {
		name   string
		mutate func(*State)
	}{
		{"存档版本", func(s *State) { s.SchemaVersion++ }},
		{"规则指纹", func(s *State) { s.RulesFingerprint = "other-rules" }},
		{"缺失规则指纹", func(s *State) { s.RulesFingerprint = "" }},
		{"零修订号", func(s *State) { s.Revision = 0 }},
		{"负修订号", func(s *State) { s.Revision = -1 }},
		{"修订号超安全整数", func(s *State) { s.Revision = maxSafeInteger + 1 }},
		{"金币低于初始值", func(s *State) { s.Coins = 1279 }},
		{"金币超安全整数", func(s *State) { s.Coins = maxSafeInteger + 1 }},
		{"负客流", func(s *State) { s.VisitorsRemaining = -1 }},
		{"客流超过日上限", func(s *State) { s.VisitorsRemaining = 51 }},
		{"负轮询游标", func(s *State) { s.NextShopIndex = -1 }},
		{"轮询游标越界", func(s *State) { s.NextShopIndex = 5 }},
		{"缺少店铺", func(s *State) { s.Shops = s.Shops[:4] }},
		{"多余店铺", func(s *State) { s.Shops = append(s.Shops, ShopState{ID: "extra"}) }},
		{"店铺ID错误", func(s *State) { s.Shops[0].ID = "unknown" }},
		{"店铺顺序变化", func(s *State) { s.Shops[0], s.Shops[1] = s.Shops[1], s.Shops[0] }},
		{"缺少结算时间", func(s *State) { s.LastAccrualAt = time.Time{} }},
		{"缺少观察时间", func(s *State) { s.LastObservedAt = time.Time{} }},
		{"结算时间晚于观察时间", func(s *State) { s.LastAccrualAt = s.LastObservedAt.Add(time.Nanosecond) }},
		{"业务日期无效", func(s *State) { s.BusinessDay = "not-a-date" }},
		{"业务日期与观察时间不符", func(s *State) { s.BusinessDay = "2026-09-11" }},
		{"结算时间不在业务日", func(s *State) { s.LastAccrualAt = s.LastAccrualAt.Add(-24 * time.Hour) }},
		{"负累计客流", func(s *State) { s.Shops[0].Visitors = -1 }},
		{"累计客流超安全收益范围", func(s *State) { s.Shops[0].Visitors = maxSafeInteger/12 + 1 }},
		{"负累计收益", func(s *State) { s.Shops[0].Revenue = -1 }},
		{"累计客流与收益不匹配", func(s *State) { s.Shops[0].Prepared = true; s.Shops[0].Visitors = 1 }},
		{"未准备店铺存在累计客流", func(s *State) { s.Shops[0].Visitors = 1; s.Shops[0].Revenue = 12; s.Coins += 12 }},
		{"金币与累计账目不匹配", func(s *State) { s.Coins++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := copyTestState(base)
			tc.mutate(&state)
			original := copyTestState(state)
			store := &memoryStore{state: state, exists: true}
			loaded, err := NewService(testConfig(t), store, testTime)
			if err == nil || loaded != nil {
				t.Fatalf("无效存档应被拒绝：%+v，%v", state, err)
			}
			if store.saveCount() != 0 {
				t.Fatal("无效存档不能被默认状态覆盖")
			}
			assertTestStoredState(t, store, original)
		})
	}
}

// TestServiceNumericLimitRollsBack 验证安全整数上限错误不发布部分结算。
func TestServiceNumericLimitRollsBack(t *testing.T) {
	for _, limit := range []string{"修订号上限", "结算中途金币上限"} {
		t.Run(limit, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Shops[0].CoinsPerVisitor = 1
			clock := &testClock{stamp: testTime()}
			seed, err := NewService(cfg, &memoryStore{}, clock.now)
			if err != nil {
				t.Fatal(err)
			}
			state := seed.Snapshot()
			state.Shops[0].Prepared = true
			if limit == "修订号上限" {
				state.Revision = maxSafeInteger
			} else {
				state.Coins = maxSafeInteger - 1
				state.Shops[0].Visitors = state.Coins - cfg.InitialCoins
				state.Shops[0].Revenue = state.Shops[0].Visitors
			}
			store := &memoryStore{state: copyTestState(state), exists: true}
			service, err := NewService(cfg, store, clock.now)
			if err != nil {
				t.Fatalf("合法上限存档应可加载：%v", err)
			}
			clock.set(testTime().Add(10 * time.Second))
			result, err := service.Settle()
			if !errors.Is(err, ErrNumericLimit) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("应拒绝超过安全整数的操作：%+v，%v", result, err)
			}
			assertTestState(t, service.Snapshot(), state)
			assertTestStoredState(t, store, state)
			if store.saveCount() != 0 {
				t.Fatal("超限结算不能写入部分结果")
			}
		})
	}
}
