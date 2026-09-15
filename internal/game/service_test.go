package game

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	return clone(state)
}

// snapshotState 暴露内部存档副本，供测试断言落盘字段（客户端另有视图）。
func (s *Service) snapshotState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.state)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadConfig(filepath.Join("..", "..", "config", "development.json"))
	if err != nil {
		t.Fatalf("读取开发配置失败：%v", err)
	}
	return cfg
}

// fundedTestConfig 只把初始金币调高到足够走完解锁与升级，其余数值仍来自开发配置。
func fundedTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.InitialCoins = 100_000
	return cfg
}

func testTime() time.Time {
	return time.Date(2026, time.September, 12, 4, 0, 0, 0, time.UTC)
}

func newTestService(t *testing.T) (*Service, *memoryStore, *testClock) {
	t.Helper()
	return newServiceWithConfig(t, testConfig(t))
}

// newFundedTestService 用于需要解锁全部铺位并升级的用例。
func newFundedTestService(t *testing.T) (*Service, *memoryStore, *testClock) {
	t.Helper()
	return newServiceWithConfig(t, fundedTestConfig(t))
}

func newServiceWithConfig(t *testing.T, cfg Config) (*Service, *memoryStore, *testClock) {
	t.Helper()
	store := &memoryStore{}
	clock := &testClock{stamp: testTime()}
	service, err := NewService(cfg, store, clock.now)
	if err != nil {
		t.Fatalf("创建本地开发服务失败：%v", err)
	}
	return service, store, clock
}

func testShopIDs(cfg Config) []string {
	ids := make([]string, 0, len(cfg.Shops))
	for _, shop := range cfg.Shops {
		ids = append(ids, shop.ID)
	}
	return ids
}

func testFloorShopIDs(cfg Config, floor int) []string {
	ids := make([]string, 0, len(cfg.Shops))
	for _, shop := range cfg.Shops {
		if shop.Floor == floor {
			ids = append(ids, shop.ID)
		}
	}
	return ids
}

// testSlotIDs 返回规范化序列化顺序（解锁顺序升序，同序保持配置顺序）的铺位 ID。
func testSlotIDs(cfg Config) []string {
	slots := append([]SlotConfig(nil), cfg.Slots...)
	sort.SliceStable(slots, func(a, b int) bool { return slots[a].UnlockOrder < slots[b].UnlockOrder })
	ids := make([]string, 0, len(slots))
	for _, slot := range slots {
		ids = append(ids, slot.ID)
	}
	return ids
}

func testShopIndex(t *testing.T, cfg Config, id string) int {
	t.Helper()
	for i, shop := range cfg.Shops {
		if shop.ID == id {
			return i
		}
	}
	t.Fatalf("配置中没有店铺 %q", id)
	return -1
}

func testSlotByID(t *testing.T, cfg Config, slotID string) SlotConfig {
	t.Helper()
	for _, slot := range cfg.Slots {
		if slot.ID == slotID {
			return slot
		}
	}
	t.Fatalf("配置中没有铺位 %q", slotID)
	return SlotConfig{}
}

func testSlotOfShop(t *testing.T, cfg Config, shopID string) SlotConfig {
	t.Helper()
	for _, slot := range cfg.Slots {
		if slot.ShopID == shopID {
			return slot
		}
	}
	t.Fatalf("配置中没有店铺 %q 对应的铺位", shopID)
	return SlotConfig{}
}

// testFloorFull 独立于实现重算满铺谓词：该层铺位全解锁且对应店铺全开业。
func testFloorFull(cfg Config, state State, floor int) bool {
	for _, slot := range cfg.Slots {
		if slot.Floor != floor {
			continue
		}
		unlocked := false
		for _, id := range state.UnlockedSlots {
			unlocked = unlocked || id == slot.ID
		}
		if !unlocked {
			return false
		}
		for i, shop := range cfg.Shops {
			if shop.ID == slot.ShopID && !state.Shops[i].Prepared {
				return false
			}
		}
	}
	return true
}

// testUnitPrice 按配置与当前状态推导单客收益，避免在断言里写死等级单价表。
func testUnitPrice(t *testing.T, cfg Config, state State, shopIndex int) int64 {
	t.Helper()
	level := state.Shops[shopIndex].Level
	price := cfg.Shops[shopIndex].LevelCoinsPerVisitor[level-1]
	if testFloorFull(cfg, state, cfg.Shops[shopIndex].Floor) {
		price += cfg.FullFloorBonus
	}
	return price
}

func testOpenShops(state State) int64 {
	var open int64
	for _, shop := range state.Shops {
		if shop.Prepared {
			open++
		}
	}
	return open
}

// testCap 返回该开业店铺数对应的客流档位。
func testCap(cfg Config, openShops int64) int64 {
	return cfg.BaseVisitors + cfg.VisitorsPerShop*openShops
}

// testRotation 独立重算轮询：从 startIndex 起跳过未开业店铺，返回总收益与各店客数。
func testRotation(t *testing.T, cfg Config, state State, startIndex int, visitors int64) (int64, []int64) {
	t.Helper()
	counts := make([]int64, len(cfg.Shops))
	var coins int64
	index := startIndex
	for served := int64(0); served < visitors; {
		if !state.Shops[index].Prepared {
			index = (index + 1) % len(cfg.Shops)
			continue
		}
		coins += testUnitPrice(t, cfg, state, index)
		counts[index]++
		served++
		index = (index + 1) % len(cfg.Shops)
	}
	return coins, counts
}

// unlockShopSlot 按解锁顺序补齐到该店铺所在铺位为止（一层铺位开局已解锁）。
func unlockShopSlot(t *testing.T, service *Service, shopID string) {
	t.Helper()
	cfg := service.Configuration()
	target := testSlotOfShop(t, cfg, shopID)
	for _, id := range testSlotIDs(cfg) {
		if testSlotByID(t, cfg, id).UnlockOrder > target.UnlockOrder {
			break
		}
		if _, err := service.Unlock(id); err != nil {
			t.Fatalf("解锁铺位 %q 失败：%v", id, err)
		}
	}
}

func prepareTestShops(t *testing.T, service *Service, ids ...string) {
	t.Helper()
	if len(ids) == 0 {
		ids = testShopIDs(service.Configuration())
	}
	for _, id := range ids {
		unlockShopSlot(t, service, id)
		if _, err := service.Prepare(id); err != nil {
			t.Fatalf("准备店铺 %q 失败：%v", id, err)
		}
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

func unlockTestSlot(t *testing.T, service *Service, slotID string) Result {
	t.Helper()
	result, err := service.Unlock(slotID)
	if err != nil {
		t.Fatalf("解锁铺位 %q 失败：%v", slotID, err)
	}
	return result
}

func upgradeTestShop(t *testing.T, service *Service, shopID string) Result {
	t.Helper()
	result, err := service.Upgrade(shopID)
	if err != nil {
		t.Fatalf("升级店铺 %q 失败：%v", shopID, err)
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

// assertTestLedger 用配置重算存档自证：逐段恒等式、聚合一致、金币恒等式与下界。
func assertTestLedger(t *testing.T, cfg Config, state State) {
	t.Helper()
	total := cfg.InitialCoins
	for i, shop := range state.Shops {
		if shop.Level < 1 || shop.Level > len(cfg.Shops[i].LevelCoinsPerVisitor) {
			t.Fatalf("店铺 %s 等级越界：%d", shop.ID, shop.Level)
		}
		var visitors, revenue int64
		for _, segment := range shop.Segments {
			if segment.Revenue != segment.Visitors*segment.UnitPrice {
				t.Fatalf("店铺 %s 分段不满足 revenue == visitors × unitPrice：%+v", shop.ID, segment)
			}
			visitors += segment.Visitors
			revenue += segment.Revenue
		}
		if shop.Visitors != visitors || shop.Revenue != revenue {
			t.Fatalf("店铺 %s 聚合值与分段不符：%+v", shop.ID, shop)
		}
		if shop.Prepared != (len(shop.Segments) > 0) {
			t.Fatalf("店铺 %s 的开业状态与分段存在性不一致：%+v", shop.ID, shop)
		}
		if !shop.Prepared && (shop.Visitors != 0 || shop.Revenue != 0) {
			t.Fatalf("未开业店铺不得有累计客流或收益：%+v", shop)
		}
		total += shop.Revenue
	}
	if state.Coins != total-state.Spent {
		t.Fatalf("金币恒等式不成立：coins=%d，initial+Σrevenue-spent=%d", state.Coins, total-state.Spent)
	}
	if state.Coins < 0 {
		t.Fatalf("金币下界不成立：%d", state.Coins)
	}
	if state.CapFrozen != (state.DailyVisitorCap != 0) {
		t.Fatalf("客流上限二态不一致：frozen=%t，cap=%d", state.CapFrozen, state.DailyVisitorCap)
	}
}

// TestServiceInitialState 验证本地开发账号的初始资源、开局铺位与客户端字段。
func TestServiceInitialState(t *testing.T) {
	service, store, _ := newTestService(t)
	cfg := testConfig(t)
	want := State{
		SchemaVersion: schemaVersion, RulesFingerprint: fingerprint(cfg), Revision: 1,
		Coins: cfg.InitialCoins, BusinessDay: "2026-09-12",
		LastAccrualAt: testTime(), LastObservedAt: testTime(),
	}
	for _, slot := range cfg.Slots {
		if slot.UnlockOrder == 0 {
			want.UnlockedSlots = append(want.UnlockedSlots, slot.ID)
		}
	}
	for _, shop := range cfg.Shops {
		want.Shops = append(want.Shops, ShopState{ID: shop.ID, Level: 1})
	}
	assertTestState(t, service.snapshotState(), want)
	assertTestStoredState(t, store, want)
	assertTestLedger(t, cfg, want)
	if store.saveCount() != 1 {
		t.Fatalf("首次初始化应仅保存一次，实际 %d 次", store.saveCount())
	}

	view := service.Snapshot()
	if view.CapFrozen || view.DailyVisitorCap != nil || view.VisitorsServed != 0 || view.VisitorsRemaining != 0 {
		t.Fatalf("新账号的客流上限必须处于未冻结状态：%+v", view)
	}
	if view.Coins != cfg.InitialCoins || view.Spent != 0 {
		t.Fatalf("视图金币或支出不符：%+v", view)
	}
	next := testSlotByID(t, cfg, testSlotIDs(cfg)[len(want.UnlockedSlots)])
	if view.NextSlotID == nil || *view.NextSlotID != next.ID {
		t.Fatalf("下一可解锁铺位错误：%+v", view.NextSlotID)
	}
	if view.NextUnlockCost == nil || *view.NextUnlockCost != next.UnlockCost {
		t.Fatalf("下一铺位解锁价格错误：%+v", view.NextUnlockCost)
	}
	for i, shop := range view.Shops {
		if shop.Level != 1 || shop.Prepared || shop.UnitPrice != testUnitPrice(t, cfg, want, i) {
			t.Fatalf("初始店铺视图错误：%+v", shop)
		}
		if shop.UpgradeCost == nil || *shop.UpgradeCost != cfg.UpgradeCosts[0] {
			t.Fatalf("初始升级成本错误：%+v", shop.UpgradeCost)
		}
	}

	assertTestResult(t, settleTestService(t, service), 0, 0, false)
	if store.saveCount() != 1 {
		t.Fatal("相同时刻重复结算不应保存")
	}
}

// TestServiceUnpreparedShopsDoNotAccrue 验证未准备期间不产出、不冻结上限且不能追补。
func TestServiceUnpreparedShopsDoNotAccrue(t *testing.T) {
	service, store, clock := newTestService(t)
	cfg := testConfig(t)
	clock.set(testTime().Add(31*time.Second + 200*time.Millisecond))
	result := settleTestService(t, service)
	assertTestResult(t, result, 0, 0, true)
	state := service.snapshotState()
	if state.Coins != cfg.InitialCoins || state.CapFrozen || state.VisitorsRemaining != 0 ||
		!state.LastAccrualAt.Equal(clock.now()) {
		t.Fatalf("未准备期间不应积累收益、冻结上限或保留可追补时间：%+v", state)
	}
	for _, shop := range state.Shops {
		if shop.Prepared || shop.Visitors != 0 || shop.Revenue != 0 || len(shop.Segments) != 0 {
			t.Fatalf("未准备店铺发生变化：%+v", shop)
		}
	}
	clock.set(testTime().Add(time.Minute))
	prepareTestShops(t, service, "coffee")
	if !service.snapshotState().LastAccrualAt.Equal(clock.now()) {
		t.Fatal("首次准备必须以准备时刻开始计时")
	}
	clock.set(testTime().Add(time.Minute + 5*time.Second - time.Nanosecond))
	assertTestResult(t, settleTestService(t, service), 0, 0, true)
	if service.snapshotState().CapFrozen {
		t.Fatal("不足一个到店间隔时不得冻结当日客流上限")
	}
	clock.set(testTime().Add(time.Minute + 5*time.Second))
	index := testShopIndex(t, cfg, "coffee")
	result = settleTestService(t, service)
	assertTestResult(t, result, testUnitPrice(t, cfg, state, index), 1, true)
	assertTestStoredState(t, store, service.snapshotState())
	assertTestLedger(t, cfg, service.snapshotState())
}

// TestServicePrepareIsIdempotent 验证重复准备不结算、不保存且不改变状态。
func TestServicePrepareIsIdempotent(t *testing.T) {
	service, store, clock := newTestService(t)
	cfg := testConfig(t)
	prepareTestShops(t, service, "coffee")
	before, saves := service.snapshotState(), store.saveCount()
	for _, stamp := range []time.Time{testTime(), testTime().Add(25 * time.Second)} {
		clock.set(stamp)
		result, err := service.Prepare("coffee")
		if err != nil {
			t.Fatal(err)
		}
		assertTestResult(t, result, 0, 0, false)
		assertTestState(t, service.snapshotState(), before)
		if store.saveCount() != saves {
			t.Fatal("重复准备不应写入存档")
		}
	}
	clock.set(testTime().Add(25 * time.Second))
	coins, _ := testRotation(t, cfg, before, 0, 5)
	assertTestResult(t, settleTestService(t, service), coins, 5, true)
}

// TestServicePrepareSettlesExistingShopsFirst 验证新增准备店铺不分享准备前的收益。
func TestServicePrepareSettlesExistingShopsFirst(t *testing.T) {
	service, _, clock := newTestService(t)
	cfg := testConfig(t)
	prepareTestShops(t, service, "coffee")
	before := service.snapshotState()
	clock.set(testTime().Add(10 * time.Second))
	result, err := service.Prepare("flowers")
	if err != nil {
		t.Fatal(err)
	}
	coins, _ := testRotation(t, cfg, before, 0, 2)
	assertTestResult(t, result, coins, 2, true)
	state := service.snapshotState()
	coffee, flowers := testShopIndex(t, cfg, "coffee"), testShopIndex(t, cfg, "flowers")
	if state.Shops[coffee].Visitors != 2 || state.Shops[flowers].Visitors != 0 || !state.Shops[flowers].Prepared {
		t.Fatalf("准备前的收益只能归已准备店铺：%+v", state.Shops)
	}
	// 满铺变化：老店保留历史分段并追加新段，新店建立唯一一条初始段。
	if len(state.Shops[coffee].Segments) != 2 || len(state.Shops[flowers].Segments) != 1 {
		t.Fatalf("满铺变化应追加分段：%+v", state.Shops)
	}
	assertTestLedger(t, cfg, state)
	clock.set(testTime().Add(15 * time.Second))
	result = settleTestService(t, service)
	assertTestResult(t, result, testUnitPrice(t, cfg, state, flowers), 1, true)
	if service.snapshotState().Shops[flowers].Visitors != 1 {
		t.Fatal("新准备店铺应参与下一次轮询")
	}
}

// TestServiceSettlementRetainsRemainder 验证服务器五秒间隔及跨请求残余时间。
func TestServiceSettlementRetainsRemainder(t *testing.T) {
	service, store, clock := newTestService(t)
	cfg := testConfig(t)
	index := testShopIndex(t, cfg, "coffee")
	prepareTestShops(t, service, "coffee")
	price := testUnitPrice(t, cfg, service.snapshotState(), index)
	steps := []struct {
		elapsed  time.Duration
		accrued  time.Duration
		visitors int64
		changed  bool
	}{
		{5*time.Second - time.Nanosecond, 0, 0, true},
		{5 * time.Second, 5 * time.Second, 1, true},
		{12500 * time.Millisecond, 10 * time.Second, 1, true},
		{15*time.Second - time.Nanosecond, 10 * time.Second, 0, true},
		{15 * time.Second, 15 * time.Second, 1, true},
		{15 * time.Second, 15 * time.Second, 0, false},
	}
	var total int64
	for _, step := range steps {
		clock.set(testTime().Add(step.elapsed))
		saves := store.saveCount()
		result := settleTestService(t, service)
		assertTestResult(t, result, step.visitors*price, step.visitors, step.changed)
		total += step.visitors
		state := service.snapshotState()
		if !state.LastAccrualAt.Equal(testTime().Add(step.accrued)) || !state.LastObservedAt.Equal(clock.now()) {
			t.Fatalf("经过 %s 后结算时间或观察时间不符：%+v", step.elapsed, state)
		}
		if state.Coins != cfg.InitialCoins+total*price {
			t.Fatalf("经过 %s 后累计资源不符：%+v", step.elapsed, state)
		}
		wantSaves := saves
		if step.changed {
			wantSaves++
		}
		if store.saveCount() != wantSaves {
			t.Fatalf("保存次数 = %d，期望 %d", store.saveCount(), wantSaves)
		}
		assertTestStoredState(t, store, state)
		assertTestLedger(t, cfg, state)
	}
}

// TestServiceRoundRobin 验证一层优先的轮询顺序与满铺加成后的每轮收益。
func TestServiceRoundRobin(t *testing.T) {
	service, _, clock := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service)
	before := service.snapshotState()
	if testOpenShops(before) != int64(len(cfg.Shops)) {
		t.Fatalf("五店应全部开业：%+v", before.Shops)
	}
	round, _ := testRotation(t, cfg, before, 0, int64(len(cfg.Shops)))
	var coins, visitors int64
	for step := 0; step < 2*len(cfg.Shops); step++ {
		index := step % len(cfg.Shops)
		clock.set(testTime().Add(time.Duration(step+1) * 5 * time.Second))
		result := settleTestService(t, service)
		assertTestResult(t, result, testUnitPrice(t, cfg, before, index), 1, true)
		coins += result.EarnedCoins
		visitors += result.VisitorsUsed
		if service.snapshotState().NextShopIndex != (index+1)%len(cfg.Shops) {
			t.Fatalf("第 %d 次轮询游标错误", step+1)
		}
	}
	if coins != 2*round || visitors != int64(2*len(cfg.Shops)) {
		t.Fatalf("两轮收益 = %d/%d，期望 %d/%d", coins, visitors, 2*round, 2*len(cfg.Shops))
	}
	state := service.snapshotState()
	if state.Coins != cfg.InitialCoins+2*round-state.Spent {
		t.Fatalf("轮询后金币错误：%+v", state)
	}
	assertTestLedger(t, cfg, state)
}

// TestServiceRoundRobinSkipsUnprepared 验证轮询跳过未准备店铺并循环回绕。
func TestServiceRoundRobinSkipsUnprepared(t *testing.T) {
	service, _, clock := newTestService(t)
	cfg := testConfig(t)
	prepareTestShops(t, service, "coffee", "flowers")
	before := service.snapshotState()
	coffee, flowers := testShopIndex(t, cfg, "coffee"), testShopIndex(t, cfg, "flowers")
	for step, index := range []int{coffee, flowers, coffee, flowers} {
		previous := service.snapshotState()
		clock.set(testTime().Add(time.Duration(step+1) * 5 * time.Second))
		result := settleTestService(t, service)
		assertTestResult(t, result, testUnitPrice(t, cfg, before, index), 1, true)
		for i, shop := range service.snapshotState().Shops {
			if i == index {
				if shop.Visitors != previous.Shops[i].Visitors+1 {
					t.Fatalf("预期轮到店铺 %s：%+v", shop.ID, service.snapshotState().Shops)
				}
			} else if shop.Visitors != previous.Shops[i].Visitors {
				t.Fatalf("非轮询店铺发生变化：%+v", shop)
			}
		}
	}
}

// TestServiceVisitorsExhausted 验证当日客流上限耗尽后不再产出。
func TestServiceVisitorsExhausted(t *testing.T) {
	service, _, clock := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service)
	before := service.snapshotState()
	capacity := testCap(cfg, testOpenShops(before))
	clock.set(testTime().Add(time.Duration(capacity)*5*time.Second + time.Second))
	result := settleTestService(t, service)
	coins, counts := testRotation(t, cfg, before, 0, capacity)
	assertTestResult(t, result, coins, capacity, true)
	state := service.snapshotState()
	if state.Coins != cfg.InitialCoins+coins-state.Spent || state.VisitorsRemaining != 0 || !state.CapFrozen {
		t.Fatalf("耗尽状态错误：%+v", state)
	}
	for i, shop := range state.Shops {
		if shop.Visitors != counts[i] {
			t.Fatalf("耗尽时店铺 %s 客数 = %d，期望 %d", shop.ID, shop.Visitors, counts[i])
		}
	}
	clock.set(clock.now().Add(time.Hour))
	result = settleTestService(t, service)
	assertTestResult(t, result, 0, 0, true)
	after := service.snapshotState()
	if after.Coins != state.Coins || after.VisitorsRemaining != state.VisitorsRemaining ||
		!reflect.DeepEqual(after.Shops, state.Shops) {
		t.Fatal("客流耗尽后不能继续产出")
	}
	if !after.LastAccrualAt.Equal(clock.now()) {
		t.Fatal("耗尽期间不应保留待补时间")
	}
}

// TestServiceReconnectSameDay 验证同日重连延续存档游标及未满五秒的残余。
func TestServiceReconnectSameDay(t *testing.T) {
	service, store, clock := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service)
	before := service.snapshotState()
	clock.set(testTime().Add(12 * time.Second))
	coins, _ := testRotation(t, cfg, before, 0, 2)
	assertTestResult(t, settleTestService(t, service), coins, 2, true)
	reconnectState, saves := service.snapshotState(), store.saveCount()
	clock.set(testTime().Add(19 * time.Second))
	reconnected, err := NewService(cfg, store, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	assertTestState(t, reconnected.snapshotState(), reconnectState)
	if store.saveCount() != saves {
		t.Fatal("加载已有存档不应自动结算或覆盖")
	}
	result := settleTestService(t, reconnected)
	assertTestResult(t, result, testUnitPrice(t, cfg, reconnectState, 2), 1, true)
	if !reconnected.snapshotState().LastAccrualAt.Equal(testTime().Add(15 * time.Second)) {
		t.Fatal("重连必须保留原有结算间隔的残余时间")
	}
}

// TestServiceShanghaiDayRefresh 验证上海跨日回到未冻结、刷新客流且保留累计账目。
func TestServiceShanghaiDayRefresh(t *testing.T) {
	start := time.Date(2026, time.September, 12, 15, 59, 50, 0, time.UTC)
	clock := &testClock{stamp: start}
	cfg := fundedTestConfig(t)
	store := &memoryStore{}
	service, err := NewService(cfg, store, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	prepareTestShops(t, service)
	before := service.snapshotState()
	clock.set(start.Add(5 * time.Second))
	assertTestResult(t, settleTestService(t, service), testUnitPrice(t, cfg, before, 0), 1, true)
	served := service.snapshotState()
	// 上海 2026-09-13 00:00:07.5：跨日并首次产生当日客人。
	midnight := start.Add(10 * time.Second)
	clock.set(midnight.Add(7500 * time.Millisecond))
	result := settleTestService(t, service)
	state := service.snapshotState()
	if state.BusinessDay != "2026-09-13" || !state.CapFrozen {
		t.Fatalf("跨日应重新冻结当日上限：%+v", state)
	}
	if state.DailyVisitorCap != testCap(cfg, int64(len(cfg.Shops))) {
		t.Fatalf("跨日后上限应取当前开业数档位：%d", state.DailyVisitorCap)
	}
	assertTestResult(t, result, testUnitPrice(t, cfg, served, 1), 1, true)
	if state.VisitorsRemaining != state.DailyVisitorCap-1 {
		t.Fatalf("跨日后剩余客流错误：%+v", state)
	}
	for i, shop := range state.Shops {
		if !shop.Prepared || shop.Visitors < served.Shops[i].Visitors {
			t.Fatalf("跨日不应清空准备状态或累计账目：%+v", shop)
		}
	}
	servedToday := int64(0)
	for i, shop := range state.Shops {
		servedToday += shop.Visitors - served.Shops[i].Visitors
	}
	if servedToday != 1 {
		t.Fatalf("跨日后只应结算当天产生的客流：%d", servedToday)
	}
	assertTestLedger(t, cfg, state)
	saves := store.saveCount()
	assertTestResult(t, settleTestService(t, service), 0, 0, false)
	if store.saveCount() != saves {
		t.Fatal("同一时刻重复请求不能再次刷新客流")
	}
}

// TestServiceOfflineDaysOnlyServeToday 验证离线多天不补算历史客流。
func TestServiceOfflineDaysOnlyServeToday(t *testing.T) {
	service, _, clock := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service)
	before := service.snapshotState()
	// 跨过四个业务日，停在次日零点之后 10 秒。
	settled := testTime().Add(4*24*time.Hour + 12*time.Hour + 10*time.Second)
	clock.set(settled)
	result := settleTestService(t, service)
	coins, _ := testRotation(t, cfg, before, 0, 2)
	assertTestResult(t, result, coins, 2, true)
	state := service.snapshotState()
	zone, err := time.LoadLocation(cfg.BusinessTimezone)
	if err != nil {
		t.Fatal(err)
	}
	if state.BusinessDay != settled.In(zone).Format(time.DateOnly) {
		t.Fatalf("业务日未按上海时区切分：%s", state.BusinessDay)
	}
	if state.VisitorsRemaining != state.DailyVisitorCap-2 {
		t.Fatalf("离线多天只应结算今天：%+v", state)
	}
	assertTestLedger(t, cfg, state)
}

// TestServiceUTCMidnightDoesNotRefresh 验证 UTC 零点不误刷新上海同日客流。
func TestServiceUTCMidnightDoesNotRefresh(t *testing.T) {
	clock := &testClock{stamp: time.Date(2026, time.September, 12, 23, 59, 55, 0, time.UTC)}
	cfg := testConfig(t)
	service, err := NewService(cfg, &memoryStore{}, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	prepareTestShops(t, service, "coffee")
	before := service.snapshotState()
	clock.set(clock.now().Add(5 * time.Second))
	assertTestResult(t, settleTestService(t, service), testUnitPrice(t, cfg, before, 0), 1, true)
	clock.set(clock.now().Add(5 * time.Second))
	result := settleTestService(t, service)
	state := service.snapshotState()
	if result.State.BusinessDay != "2026-09-13" || !state.CapFrozen || state.VisitorsRemaining != state.DailyVisitorCap-2 {
		t.Fatalf("UTC 跨日不应刷新上海同日客流：%+v", state)
	}
}

// TestServiceExhaustedVisitorsRefreshNextDay 验证客流耗尽后在次日恢复并保留累计收益。
func TestServiceExhaustedVisitorsRefreshNextDay(t *testing.T) {
	service, store, clock := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service)
	before := service.snapshotState()
	capacity := testCap(cfg, testOpenShops(before))
	clock.set(testTime().Add(time.Hour))
	coins, _ := testRotation(t, cfg, before, 0, capacity)
	assertTestResult(t, settleTestService(t, service), coins, capacity, true)
	after := service.snapshotState()
	clock.set(testTime().Add(12*time.Hour + 10*time.Second))
	result := settleTestService(t, service)
	nextCoins, counts := testRotation(t, cfg, after, after.NextShopIndex, 2)
	assertTestResult(t, result, nextCoins, 2, true)
	state := service.snapshotState()
	if state.VisitorsRemaining != state.DailyVisitorCap-2 || state.BusinessDay != "2026-09-13" {
		t.Fatalf("耗尽后应补充次日客流：%+v", state)
	}
	for i, shop := range state.Shops {
		if shop.Visitors != after.Shops[i].Visitors+counts[i] {
			t.Fatalf("刷新不能清空累计账目：%+v", shop)
		}
	}
	assertTestStoredState(t, store, state)
	assertTestLedger(t, cfg, state)
}

// TestServiceUnpreparedAcrossDays 验证跨日后首次准备不追补当天或历史未营业收益。
func TestServiceUnpreparedAcrossDays(t *testing.T) {
	service, _, clock := newTestService(t)
	cfg := testConfig(t)
	clock.set(testTime().Add(72 * time.Hour))
	prepareTestShops(t, service, "coffee")
	state := service.snapshotState()
	if state.BusinessDay != "2026-09-15" || state.Coins != cfg.InitialCoins || state.CapFrozen ||
		!state.LastAccrualAt.Equal(clock.now()) {
		t.Fatalf("跨日未准备期间不能追补收益：%+v", state)
	}
	clock.set(clock.now().Add(5 * time.Second))
	index := testShopIndex(t, cfg, "coffee")
	assertTestResult(t, settleTestService(t, service), testUnitPrice(t, cfg, state, index), 1, true)
}

// TestUnlockChargesConfiguredCost 验证解锁按配置扣费、写入解锁集合与累计支出。
func TestUnlockChargesConfiguredCost(t *testing.T) {
	service, store, _ := newTestService(t)
	cfg := testConfig(t)
	target := cfg.Slots[2]
	before := service.snapshotState()
	result := unlockTestSlot(t, service, target.ID)
	assertTestResult(t, result, 0, 0, true)
	state := service.snapshotState()
	if state.Coins != before.Coins-target.UnlockCost || state.Spent != target.UnlockCost {
		t.Fatalf("解锁扣费错误：%+v", state)
	}
	want := []string{cfg.Slots[0].ID, cfg.Slots[1].ID, target.ID}
	if !reflect.DeepEqual(state.UnlockedSlots, want) {
		t.Fatalf("解锁集合错误：%v", state.UnlockedSlots)
	}
	if state.CapFrozen || state.VisitorsRemaining != 0 {
		t.Fatalf("解锁不得改变当天客流上限：%+v", state)
	}
	assertTestStoredState(t, store, state)
	assertTestLedger(t, cfg, state)
	// 解锁后该铺位变为「待开业」，可以开店。
	if _, err := service.Prepare(target.ShopID); err != nil {
		t.Fatalf("解锁后应可开店：%v", err)
	}
}

// TestUnlockRequiresCoins 验证金币不足时解锁失败且零变化（验收 #1）。
func TestUnlockRequiresCoins(t *testing.T) {
	service, store, _ := newTestService(t)
	cfg := testConfig(t)
	// 先买下第一个二层铺位，剩下的金币不足以买第二个。
	first := cfg.Slots[2]
	unlockTestSlot(t, service, first.ID)
	second := cfg.Slots[3]
	if second.UnlockCost <= service.snapshotState().Coins {
		t.Fatalf("测试前提不成立：解锁 %s 后仍买得起 %s", first.ID, second.ID)
	}
	before, saves := service.snapshotState(), store.saveCount()
	result, err := service.Unlock(second.ID)
	if !errors.Is(err, ErrInsufficientCoins) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("金币不足应返回 INSUFFICIENT_COINS：%+v，%v", result, err)
	}
	assertTestState(t, service.snapshotState(), before)
	assertTestStoredState(t, store, before)
	if store.saveCount() != saves {
		t.Fatal("失败的解锁不能写入存档")
	}
}

// TestUnlockRejectsSkippingOrder 验证跳序解锁被拒绝且零变化（验收 #2）。
func TestUnlockRejectsSkippingOrder(t *testing.T) {
	service, store, _ := newFundedTestService(t)
	cfg := service.Configuration()
	before, saves := service.snapshotState(), store.saveCount()
	for _, slot := range cfg.Slots[3:] { // 未解锁衣橱先解锁甜屋 / 书店
		result, err := service.Unlock(slot.ID)
		if !errors.Is(err, ErrSlotOrder) || !reflect.DeepEqual(result, Result{}) {
			t.Fatalf("跳序解锁 %s 应返回 SLOT_ORDER：%+v，%v", slot.ID, result, err)
		}
		assertTestState(t, service.snapshotState(), before)
		if store.saveCount() != saves {
			t.Fatal("跳序解锁不能写入存档")
		}
	}
	// 补齐前序后即可依次解锁，最后全部解锁时不再给出下一铺位。
	for _, slot := range cfg.Slots[2:] {
		unlockTestSlot(t, service, slot.ID)
	}
	if view := service.Snapshot(); view.NextSlotID != nil || view.NextUnlockCost != nil {
		t.Fatalf("全部解锁后不得再给出下一铺位：%+v", view)
	}
}

// TestUnlockIsIdempotent 验证重复解锁为幂等成功（验收 #3）。
func TestUnlockIsIdempotent(t *testing.T) {
	service, store, clock := newTestService(t)
	cfg := testConfig(t)
	// 一层铺位开局已解锁，重复解锁必须零副作用。
	for _, id := range []string{cfg.Slots[0].ID, cfg.Slots[1].ID} {
		before, saves := service.snapshotState(), store.saveCount()
		clock.set(testTime().Add(30 * time.Second))
		result, err := service.Unlock(id)
		if err != nil {
			t.Fatal(err)
		}
		assertTestResult(t, result, 0, 0, false)
		assertTestState(t, service.snapshotState(), before)
		if store.saveCount() != saves {
			t.Fatal("重复解锁不应写入存档")
		}
	}
	target := cfg.Slots[2]
	first := unlockTestSlot(t, service, target.ID)
	before, saves := service.snapshotState(), store.saveCount()
	second := unlockTestSlot(t, service, target.ID)
	if second.Changed || second.State.Coins != first.State.Coins || second.State.Spent != target.UnlockCost {
		t.Fatalf("重复解锁必须幂等：%+v", second)
	}
	assertTestState(t, service.snapshotState(), before)
	if store.saveCount() != saves {
		t.Fatal("重复解锁不应写入存档")
	}
}

// TestUnlockRejectsUnknownSlot 验证未知铺位 ID 不产生任何副作用。
func TestUnlockRejectsUnknownSlot(t *testing.T) {
	service, store, clock := newTestService(t)
	before, saves := service.snapshotState(), store.saveCount()
	clock.set(testTime().Add(48 * time.Hour))
	for _, id := range []string{"", "f3-s1", "f2-s4", "F2-S1", "f2-s1 "} {
		result, err := service.Unlock(id)
		if !errors.Is(err, ErrUnknownSlot) || !reflect.DeepEqual(result, Result{}) {
			t.Fatalf("未知铺位 %q 返回结果错误：%+v，%v", id, result, err)
		}
		assertTestState(t, service.snapshotState(), before)
		assertTestStoredState(t, store, before)
		if store.saveCount() != saves {
			t.Fatal("未知铺位请求不能保存状态")
		}
	}
}

// TestPrepareRejectsLockedSlot 验证对未解锁铺位开店返回 SLOT_LOCKED（验收 #7）。
func TestPrepareRejectsLockedSlot(t *testing.T) {
	service, store, _ := newTestService(t)
	cfg := testConfig(t)
	before, saves := service.snapshotState(), store.saveCount()
	for _, slot := range cfg.Slots[2:] {
		result, err := service.Prepare(slot.ShopID)
		if !errors.Is(err, ErrSlotLocked) || !reflect.DeepEqual(result, Result{}) {
			t.Fatalf("未解锁铺位开店应返回 SLOT_LOCKED：%+v，%v", result, err)
		}
		assertTestState(t, service.snapshotState(), before)
	}
	if store.saveCount() != saves {
		t.Fatal("未解锁铺位开店不能写入存档")
	}
	// 未解锁铺位不得出现累计客流、收益或分段。
	for i, shop := range service.snapshotState().Shops {
		if cfg.Shops[i].Floor == 2 && (shop.Visitors != 0 || shop.Revenue != 0 || len(shop.Segments) != 0) {
			t.Fatalf("未解锁铺位不得有账目：%+v", shop)
		}
	}
}

// TestUpgradeRequiresOpenShop 验证对「待开业」店铺升级被拒绝（验收 #8）。
func TestUpgradeRequiresOpenShop(t *testing.T) {
	service, store, _ := newTestService(t)
	cfg := testConfig(t)
	unlockTestSlot(t, service, cfg.Slots[2].ID)
	before, saves := service.snapshotState(), store.saveCount()
	result, err := service.Upgrade(cfg.Slots[2].ShopID)
	if !errors.Is(err, ErrShopNotOpen) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("待开业店铺升级应返回 SHOP_NOT_OPEN：%+v，%v", result, err)
	}
	assertTestState(t, service.snapshotState(), before)
	assertTestStoredState(t, store, before)
	if store.saveCount() != saves {
		t.Fatal("被拒绝的升级不能写入存档")
	}
	// 未解锁铺位的店铺同样不能升级，但原因码是「先解锁」。
	result, err = service.Upgrade(cfg.Slots[3].ShopID)
	if !errors.Is(err, ErrSlotLocked) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("未解锁铺位的店铺升级应返回 SLOT_LOCKED：%+v，%v", result, err)
	}
	assertTestState(t, service.snapshotState(), before)
}

// TestUpgradeChangesPriceAndKeepsHistory 验证升级只影响此后收益（验收 #4）。
func TestUpgradeChangesPriceAndKeepsHistory(t *testing.T) {
	service, store, clock := newFundedTestService(t)
	cfg := service.Configuration()
	index := testShopIndex(t, cfg, "coffee")
	prepareTestShops(t, service, "coffee")
	clock.set(testTime().Add(15 * time.Second))
	first := settleTestService(t, service)
	assertTestResult(t, first, 3*testUnitPrice(t, cfg, service.snapshotState(), index), 3, true)
	beforeUpgrade := service.snapshotState()
	upgraded := upgradeTestShop(t, service, "coffee")
	afterUpgrade := service.snapshotState()
	if afterUpgrade.Shops[index].Level != 2 || afterUpgrade.Spent != beforeUpgrade.Spent+cfg.UpgradeCosts[0] {
		t.Fatalf("升级未生效：%+v", afterUpgrade.Shops[index])
	}
	if afterUpgrade.Coins != beforeUpgrade.Coins-cfg.UpgradeCosts[0] {
		t.Fatalf("升级扣费错误：%d", afterUpgrade.Coins)
	}
	// 历史分段原样保留，只追加一段新价。
	if len(afterUpgrade.Shops[index].Segments) != 2 {
		t.Fatalf("升级应追加一条分段：%+v", afterUpgrade.Shops[index].Segments)
	}
	if !reflect.DeepEqual(afterUpgrade.Shops[index].Segments[0], beforeUpgrade.Shops[index].Segments[0]) {
		t.Fatal("升级不能改写历史分段")
	}
	if got, want := afterUpgrade.Shops[index].Segments[1].UnitPrice, testUnitPrice(t, cfg, afterUpgrade, index); got != want {
		t.Fatalf("新分段单价 = %d，期望 %d", got, want)
	}
	if upgraded.EarnedCoins != 0 || upgraded.VisitorsUsed != 0 {
		t.Fatal("升级本身不产生收益")
	}
	if view := service.Snapshot(); view.Shops[index].UnitPrice != testUnitPrice(t, cfg, afterUpgrade, index) {
		t.Fatalf("升级后接口单价未更新：%+v", view.Shops[index])
	}
	assertTestLedger(t, cfg, afterUpgrade)
	// 此后收益按新等级单价记账。
	clock.set(clock.now().Add(5 * time.Second))
	result := settleTestService(t, service)
	assertTestResult(t, result, testUnitPrice(t, cfg, afterUpgrade, index), 1, true)
	assertTestLedger(t, cfg, service.snapshotState())
	assertTestStoredState(t, store, service.snapshotState())
}

// TestUpgradeMaxLevelAndCoins 验证满级与金币不足的拒绝及零变化。
func TestUpgradeMaxLevelAndCoins(t *testing.T) {
	service, store, _ := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service, "coffee")
	levels := len(cfg.Shops[0].LevelCoinsPerVisitor)
	for level := 1; level < levels; level++ {
		upgradeTestShop(t, service, "coffee")
	}
	if service.snapshotState().Shops[0].Level != levels {
		t.Fatalf("升级次数错误：%d", service.snapshotState().Shops[0].Level)
	}
	before, saves := service.snapshotState(), store.saveCount()
	result, err := service.Upgrade("coffee")
	if !errors.Is(err, ErrMaxLevel) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("满级再升级应返回 MAX_LEVEL：%+v，%v", result, err)
	}
	assertTestState(t, service.snapshotState(), before)
	if store.saveCount() != saves {
		t.Fatal("满级请求不能写入存档")
	}
	if view := service.Snapshot(); view.Shops[0].UpgradeCost != nil {
		t.Fatalf("满级店铺的升级成本必须为 null：%+v", view.Shops[0])
	}
	// 金币不足：一层铺位开局已解锁，但余额为零。
	poorConfig := testConfig(t)
	poorConfig.InitialCoins = 0
	poor, poorStore, _ := newServiceWithConfig(t, poorConfig)
	if _, err := poor.Prepare("coffee"); err != nil {
		t.Fatal(err)
	}
	before, saves = poor.snapshotState(), poorStore.saveCount()
	result, err = poor.Upgrade("coffee")
	if !errors.Is(err, ErrInsufficientCoins) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("金币不足应返回 INSUFFICIENT_COINS：%+v，%v", result, err)
	}
	assertTestState(t, poor.snapshotState(), before)
	if poorStore.saveCount() != saves {
		t.Fatal("失败的升级不能写入存档")
	}
}

// TestPrepareCreatesFirstSegment 验证开店建立首个分段（验收 #4 开店建段）。
func TestPrepareCreatesFirstSegment(t *testing.T) {
	service, _, _ := newTestService(t)
	cfg := testConfig(t)
	index := testShopIndex(t, cfg, "coffee")
	if _, err := service.Prepare("coffee"); err != nil {
		t.Fatal(err)
	}
	state := service.snapshotState()
	if len(state.Shops[index].Segments) != 1 {
		t.Fatalf("开店必须建立唯一一条初始分段：%+v", state.Shops[index].Segments)
	}
	segment := state.Shops[index].Segments[0]
	if segment != (Segment{UnitPrice: testUnitPrice(t, cfg, state, index), Visitors: 0, Revenue: 0}) {
		t.Fatalf("初始分段错误：%+v", segment)
	}
	assertTestLedger(t, cfg, state)
}

// TestPrepareTriggeringFullFloorAddsSingleBonusSegment 验证触发满铺的新开业店只产生一条 bonus 价段。
func TestPrepareTriggeringFullFloorAddsSingleBonusSegment(t *testing.T) {
	service, _, _ := newTestService(t)
	cfg := testConfig(t)
	floor := cfg.Shops[0].Floor
	floorShops := testFloorShopIDs(cfg, floor)
	last := floorShops[len(floorShops)-1]
	// 先把该层除最后一个铺位以外的店铺开起来。
	for _, id := range floorShops[:len(floorShops)-1] {
		if _, err := service.Prepare(id); err != nil {
			t.Fatal(err)
		}
	}
	before := service.snapshotState()
	if testFloorFull(cfg, before, floor) {
		t.Fatal("测试前提不成立：该层不应已经满铺")
	}
	if _, err := service.Prepare(last); err != nil {
		t.Fatal(err)
	}
	state := service.snapshotState()
	if !testFloorFull(cfg, state, floor) {
		t.Fatalf("该层应已满铺：%+v", state.UnlockedSlots)
	}
	index := testShopIndex(t, cfg, last)
	segments := state.Shops[index].Segments
	bonus := cfg.Shops[index].LevelCoinsPerVisitor[0] + cfg.FullFloorBonus
	if len(segments) != 1 {
		t.Fatalf("触发满铺的新开业店只能有一条分段，实际 %+v", segments)
	}
	if segments[0] != (Segment{UnitPrice: bonus, Visitors: 0, Revenue: 0}) {
		t.Fatalf("触发满铺的首段必须直接是 bonus 价 %d：%+v", bonus, segments[0])
	}
	// 同层其他已开业店各追加一条新段，旧段原样保留。
	for _, id := range floorShops[:len(floorShops)-1] {
		other := testShopIndex(t, cfg, id)
		otherSegments := state.Shops[other].Segments
		if len(otherSegments) != 2 {
			t.Fatalf("同层已开业店应追加一条分段：%+v", otherSegments)
		}
		if otherSegments[0].UnitPrice != cfg.Shops[other].LevelCoinsPerVisitor[0] {
			t.Fatalf("旧段单价应保持不变：%+v", otherSegments[0])
		}
		if want := testUnitPrice(t, cfg, state, other); otherSegments[1].UnitPrice != want {
			t.Fatalf("新段单价 = %d，期望 bonus 价 %d", otherSegments[1].UnitPrice, want)
		}
	}
	assertTestLedger(t, cfg, state)
}

// TestEmptySegmentsAreRecorded 验证空段允许存在且逐段恒等式成立（验收 #4 空段）。
func TestEmptySegmentsAreRecorded(t *testing.T) {
	service, _, clock := newFundedTestService(t)
	cfg := service.Configuration()
	index := testShopIndex(t, cfg, "coffee")
	prepareTestShops(t, service, "coffee")
	upgradeTestShop(t, service, "coffee")
	upgradeTestShop(t, service, "coffee")
	state := service.snapshotState()
	segments := state.Shops[index].Segments
	if len(segments) != 3 {
		t.Fatalf("两次升级应各追加一条空段：%+v", segments)
	}
	for _, segment := range segments[1:] {
		if segment.Visitors != 0 || segment.Revenue != 0 {
			t.Fatalf("无客流期间升级必须留下空段：%+v", segment)
		}
		if segment.Revenue != segment.Visitors*segment.UnitPrice {
			t.Fatalf("空段必须满足 0 == 0 × unitPrice：%+v", segment)
		}
	}
	if state.Shops[index].Visitors != 0 || state.Shops[index].Revenue != 0 {
		t.Fatal("空段不得计入累计客流或收益")
	}
	assertTestLedger(t, cfg, state)
	// 空段不破坏存档自证，也不影响此后结算。
	clock.set(clock.now().Add(5 * time.Second))
	result := settleTestService(t, service)
	assertTestResult(t, result, testUnitPrice(t, cfg, state, index), 1, true)
	assertTestLedger(t, cfg, service.snapshotState())
}

// TestSegmentsStayBounded 验证每店分段数不超过 1 初始 + 升级数 + 1 满铺。
func TestSegmentsStayBounded(t *testing.T) {
	service, _, _ := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service)
	index := testShopIndex(t, cfg, "coffee")
	levels := len(cfg.Shops[index].LevelCoinsPerVisitor)
	for level := 1; level < levels; level++ {
		upgradeTestShop(t, service, "coffee")
	}
	state := service.snapshotState()
	if got, limit := len(state.Shops[index].Segments), levels+1; got != limit {
		t.Fatalf("分段数 = %d，期望 %d（1 初始 + %d 升级 + 1 满铺）", got, limit, levels-1)
	}
	assertTestLedger(t, cfg, state)
}

// TestFloorBonusAppliesOnlyWhenFloorIsFull 验证满铺加成的生效条件（验收 #6）。
func TestFloorBonusAppliesOnlyWhenFloorIsFull(t *testing.T) {
	service, _, _ := newFundedTestService(t)
	cfg := service.Configuration()
	floor := cfg.Shops[0].Floor
	floorShops := testFloorShopIDs(cfg, floor)
	first := testShopIndex(t, cfg, floorShops[0])
	// 只开一家：该层未满铺，不含加成。
	if _, err := service.Prepare(floorShops[0]); err != nil {
		t.Fatal(err)
	}
	state := service.snapshotState()
	if testFloorFull(cfg, state, floor) {
		t.Fatal("只开一家时该层不可能满铺")
	}
	if got := service.Snapshot().Shops[first].UnitPrice; got != cfg.Shops[first].LevelCoinsPerVisitor[0] {
		t.Fatalf("未满铺时单价 = %d，不应包含加成", got)
	}
	// 补齐该层：加成生效。
	if _, err := service.Prepare(floorShops[1]); err != nil {
		t.Fatal(err)
	}
	state = service.snapshotState()
	if !testFloorFull(cfg, state, floor) {
		t.Fatal("同层全部营业中时应满铺")
	}
	if got, want := service.Snapshot().Shops[first].UnitPrice, cfg.Shops[first].LevelCoinsPerVisitor[0]+cfg.FullFloorBonus; got != want {
		t.Fatalf("满铺后单价 = %d，期望 %d", got, want)
	}
	// 另一层仍有未开放铺位，其加成不生效。
	otherFloor := 3 - floor
	for i, shop := range cfg.Shops {
		if shop.Floor == otherFloor && service.Snapshot().Shops[i].UnitPrice != cfg.Shops[i].LevelCoinsPerVisitor[0] {
			t.Fatalf("未满铺楼层不得有加成：%+v", service.Snapshot().Shops[i])
		}
	}
	assertTestLedger(t, cfg, state)
}

// TestVisitorCapFreezesOnFirstServedSettle 验证客流上限的冻结时机（验收 #5）。
func TestVisitorCapFreezesOnFirstServedSettle(t *testing.T) {
	service, store, clock := newTestService(t)
	cfg := testConfig(t)
	// 0 店开业：反复结算都不冻结、不落盘。
	clock.set(testTime().Add(30 * time.Second))
	assertTestResult(t, settleTestService(t, service), 0, 0, true)
	clock.set(testTime().Add(60 * time.Second))
	assertTestResult(t, settleTestService(t, service), 0, 0, true)
	if state := service.snapshotState(); state.CapFrozen || state.DailyVisitorCap != 0 || state.VisitorsRemaining != 0 {
		t.Fatalf("0 店开业不得冻结上限：%+v", state)
	}
	if view := service.Snapshot(); view.CapFrozen || view.DailyVisitorCap != nil || view.VisitorsServed != 0 {
		t.Fatalf("未冻结时接口必须返回 null 上限：%+v", view)
	}
	// 开店但不足一个间隔：仍不冻结。
	prepareTestShops(t, service, "coffee")
	clock.set(clock.now().Add(5*time.Second - time.Nanosecond))
	assertTestResult(t, settleTestService(t, service), 0, 0, true)
	if service.snapshotState().CapFrozen {
		t.Fatal("不足一个到店间隔不得冻结")
	}
	// 首次实际产生客人：冻结为「基础 + 单店 × 开业数」。
	clock.set(clock.now().Add(time.Nanosecond))
	index := testShopIndex(t, cfg, "coffee")
	before := service.snapshotState()
	result := settleTestService(t, service)
	assertTestResult(t, result, testUnitPrice(t, cfg, before, index), 1, true)
	state := service.snapshotState()
	if !state.CapFrozen || state.DailyVisitorCap != testCap(cfg, 1) {
		t.Fatalf("首次产生客人应冻结为 1 店档位：%+v", state)
	}
	if state.VisitorsRemaining != state.DailyVisitorCap-1 {
		t.Fatalf("冻结后剩余客流错误：%+v", state)
	}
	view := service.Snapshot()
	if view.DailyVisitorCap == nil || *view.DailyVisitorCap != state.DailyVisitorCap || view.VisitorsServed != 1 {
		t.Fatalf("冻结后接口字段错误：%+v", view)
	}
	assertTestStoredState(t, store, state)
	assertTestLedger(t, cfg, state)
}

// TestOpenShopMidDayDoesNotChangeCap 验证当日中途开店不改变已冻结的上限（验收 #5）。
func TestOpenShopMidDayDoesNotChangeCap(t *testing.T) {
	service, _, clock := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service, "coffee")
	clock.set(testTime().Add(5 * time.Second))
	settleTestService(t, service)
	frozen := service.snapshotState()
	if frozen.DailyVisitorCap != testCap(cfg, 1) {
		t.Fatalf("冻结档位错误：%d", frozen.DailyVisitorCap)
	}
	for _, id := range []string{"flowers", "clothing", "dessert", "bookstore"} {
		prepareTestShops(t, service, id)
		if state := service.snapshotState(); state.DailyVisitorCap != frozen.DailyVisitorCap {
			t.Fatalf("中途开店不得改变当日上限：%d → %d", frozen.DailyVisitorCap, state.DailyVisitorCap)
		}
	}
	// 次日按新的开业数重新冻结。
	clock.set(testTime().Add(24 * time.Hour))
	result := settleTestService(t, service)
	if !result.Changed {
		t.Fatal("次日应重新冻结上限")
	}
	if state := service.snapshotState(); state.DailyVisitorCap != testCap(cfg, int64(len(cfg.Shops))) {
		t.Fatalf("次日上限应按当前开业数：%d", state.DailyVisitorCap)
	}
	assertTestLedger(t, cfg, service.snapshotState())
}

// TestConcurrentUnlockSameSlot 验证并发解锁同一铺位只提交一次、只扣一次费（验收 #9a）。
func TestConcurrentUnlockSameSlot(t *testing.T) {
	service, store, _ := newTestService(t)
	cfg := testConfig(t)
	target := cfg.Slots[2]
	before := service.snapshotState()
	results, rejected := concurrentTestOperations(t, 64, func(int) (Result, error) { return service.Unlock(target.ID) })
	changed := 0
	for _, result := range results {
		if result.Changed {
			changed++
		}
	}
	state := service.snapshotState()
	if changed != 1 || rejected != 0 || state.Spent != target.UnlockCost || state.Coins != before.Coins-target.UnlockCost {
		t.Fatalf("并发解锁应只提交一次、只扣一次费：changed=%d，rejected=%d，state=%+v", changed, rejected, state)
	}
	if store.saveCount() != 2 || state.Revision != 2 {
		t.Fatalf("并发解锁应只写一次存档：saves=%d，revision=%d", store.saveCount(), state.Revision)
	}
	assertTestLedger(t, cfg, state)
}

// TestConcurrentUpgradeSameShop 验证并发升级同一店铺只提交一次、只扣一次费（验收 #9b）。
func TestConcurrentUpgradeSameShop(t *testing.T) {
	service, _, _ := newFundedTestService(t)
	cfg := service.Configuration()
	index := testShopIndex(t, cfg, "coffee")
	prepareTestShops(t, service, "coffee")
	// 把余额压到恰好一次升级成本，金币恒等式仍然成立。
	state := service.snapshotState()
	state.Spent += state.Coins - cfg.UpgradeCosts[0]
	state.Coins = cfg.UpgradeCosts[0]
	store := &memoryStore{state: copyTestState(state), exists: true}
	service, err := NewService(cfg, store, func() time.Time { return testTime() })
	if err != nil {
		t.Fatalf("按精确余额重建服务失败：%v", err)
	}
	before := service.snapshotState()
	results, rejected := concurrentTestOperations(t, 64, func(int) (Result, error) { return service.Upgrade("coffee") })
	changed := 0
	for _, result := range results {
		if result.Changed {
			changed++
		}
	}
	state = service.snapshotState()
	if changed != 1 || rejected != 63 || state.Shops[index].Level != 2 ||
		state.Spent != before.Spent+cfg.UpgradeCosts[0] || state.Coins != 0 {
		t.Fatalf("并发升级应只提交一次、只扣一次费：changed=%d，rejected=%d，state=%+v", changed, rejected, state)
	}
	if store.saveCount() != 1 || state.Revision != before.Revision+1 {
		t.Fatalf("并发升级应只写一次存档：saves=%d，revision=%d", store.saveCount(), state.Revision)
	}
	assertTestLedger(t, cfg, state)
}

// TestConcurrentUnlockAndUpgrade 验证并发解锁与升级互不干扰、账目守恒（验收 #9c）。
func TestConcurrentUnlockAndUpgrade(t *testing.T) {
	service, _, _ := newFundedTestService(t)
	cfg := service.Configuration()
	index := testShopIndex(t, cfg, "coffee")
	prepareTestShops(t, service, "coffee")
	before := service.snapshotState()
	target := cfg.Slots[2]
	results, _ := concurrentTestOperations(t, 64, func(i int) (Result, error) {
		if i%2 == 0 {
			return service.Unlock(target.ID)
		}
		return service.Upgrade("coffee")
	})
	state := service.snapshotState()
	unlocked := int64(0)
	for _, id := range state.UnlockedSlots {
		if id == target.ID {
			unlocked++
		}
	}
	upgrades := int64(state.Shops[index].Level - before.Shops[index].Level)
	wantSpent := target.UnlockCost * unlocked
	for i := int64(0); i < upgrades; i++ {
		wantSpent += cfg.UpgradeCosts[i]
	}
	if unlocked != 1 || upgrades < 1 || state.Spent != wantSpent {
		t.Fatalf("并发解锁与升级扣费错误：unlocked=%d，upgrades=%d，state=%+v", unlocked, upgrades, state)
	}
	if state.Coins != cfg.InitialCoins+state.Shops[index].Revenue-state.Spent {
		t.Fatalf("并发操作后金币不守恒：%+v", state)
	}
	// 提交次数与存档写入必须一一对应，不能丢更新。
	changed := 0
	for _, result := range results {
		if result.Changed {
			changed++
		}
	}
	if state.Revision != before.Revision+int64(changed) {
		t.Fatalf("提交次数与修订号不符：changed=%d，revision=%d", changed, state.Revision)
	}
	assertTestLedger(t, cfg, state)
}

// TestConcurrentCommandsShareOneLock 验证解锁、升级、开店与结算共用同一把锁（验收 #9d）。
func TestConcurrentCommandsShareOneLock(t *testing.T) {
	service, store, clock := newFundedTestService(t)
	cfg := service.Configuration()
	index := testShopIndex(t, cfg, "coffee")
	clock.set(testTime().Add(25 * time.Second))
	results, _ := concurrentTestOperations(t, 96, func(i int) (Result, error) {
		switch i % 4 {
		case 0:
			return service.Prepare("coffee")
		case 1:
			return service.Unlock(cfg.Slots[2].ID)
		case 2:
			return service.Upgrade("coffee")
		default:
			return service.Settle()
		}
	})
	changed := 0
	for _, result := range results {
		if result.Changed {
			changed++
		}
	}
	state := service.snapshotState()
	// 25 秒最多产出 5 位客人，且只能由一次结算提交。
	if state.Shops[index].Visitors > 5 {
		t.Fatalf("并发结算重复产出：%+v", state.Shops[index])
	}
	if state.CapFrozen && state.VisitorsRemaining != state.DailyVisitorCap-state.Shops[index].Visitors {
		t.Fatalf("已到店数与剩余客流不一致：%+v", state)
	}
	if state.Shops[index].Level > len(cfg.Shops[index].LevelCoinsPerVisitor) {
		t.Fatalf("升级次数越界：%d", state.Shops[index].Level)
	}
	// 每次提交都必须完整落盘：修订号与存档写入次数一一对应。
	if state.Revision != int64(1+changed) || store.saveCount() != int(state.Revision) {
		t.Fatalf("提交次数、修订号与存档写入不一致：changed=%d，revision=%d，saves=%d",
			changed, state.Revision, store.saveCount())
	}
	assertTestLedger(t, cfg, state)
}

// TestSaveSelfProvesAfterSpending 验证消费后存档仍能自证并正常加载（验收 #10）。
func TestSaveSelfProvesAfterSpending(t *testing.T) {
	service, store, clock := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service)
	clock.set(testTime().Add(50 * time.Second))
	settleTestService(t, service)
	// 解锁 + 升级：coins 低于 initialCoins，但仍必须通过自证。
	unlockTestSlot(t, service, cfg.Slots[2].ID)
	upgradeTestShop(t, service, "coffee")
	state := service.snapshotState()
	if state.Coins >= cfg.InitialCoins {
		t.Fatalf("测试前提不成立：消费后金币未低于初始值：%d", state.Coins)
	}
	assertTestLedger(t, cfg, state)
	assertTestStoredState(t, store, state)
	// 以该存档重启必须成功（M01 的 coins >= initialCoins 会误杀）。
	reloaded, err := NewService(cfg, store, clock.now)
	if err != nil {
		t.Fatalf("消费后存档必须仍可加载：%v", err)
	}
	assertTestState(t, reloaded.snapshotState(), state)
	assertTestLedger(t, cfg, reloaded.snapshotState())
}

// TestServiceRejectsClockBackwards 验证结算及首次准备拒绝服务器时钟回退。
func TestServiceRejectsClockBackwards(t *testing.T) {
	for _, operation := range []string{"结算", "准备"} {
		for _, rollback := range []time.Duration{time.Nanosecond, 24 * time.Hour} {
			t.Run(operation+"/"+rollback.String(), func(t *testing.T) {
				service, store, clock := newTestService(t)
				prepareTestShops(t, service, "coffee")
				clock.set(testTime().Add(12 * time.Second))
				settleTestService(t, service)
				before, saves := service.snapshotState(), store.saveCount()
				clock.set(before.LastObservedAt.Add(-rollback))
				var result Result
				var err error
				if operation == "准备" {
					result, err = service.Prepare("flowers")
				} else {
					result, err = service.Settle()
				}
				if !errors.Is(err, ErrClockBackwards) {
					t.Fatalf("期望时钟回退错误，实际 %v", err)
				}
				if !reflect.DeepEqual(result, Result{}) {
					t.Fatalf("失败操作不能返回已提交结果：%+v", result)
				}
				assertTestState(t, service.snapshotState(), before)
				assertTestStoredState(t, store, before)
				if store.saveCount() != saves {
					t.Fatal("时钟回退不能保存状态")
				}
			})
		}
	}
}

// TestUnlockAndUpgradeRejectClockBackwards 验证解锁与升级同样拒绝时钟回退。
func TestUnlockAndUpgradeRejectClockBackwards(t *testing.T) {
	for _, operation := range []string{"解锁", "升级"} {
		t.Run(operation, func(t *testing.T) {
			service, store, clock := newFundedTestService(t)
			cfg := service.Configuration()
			prepareTestShops(t, service, "coffee")
			clock.set(testTime().Add(12 * time.Second))
			settleTestService(t, service)
			before, saves := service.snapshotState(), store.saveCount()
			clock.set(before.LastObservedAt.Add(-time.Second))
			var result Result
			var err error
			if operation == "解锁" {
				result, err = service.Unlock(cfg.Slots[2].ID)
			} else {
				result, err = service.Upgrade("coffee")
			}
			if !errors.Is(err, ErrClockBackwards) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("期望时钟回退错误：%+v，%v", result, err)
			}
			assertTestState(t, service.snapshotState(), before)
			assertTestStoredState(t, store, before)
			if store.saveCount() != saves {
				t.Fatal("时钟回退不能保存状态")
			}
		})
	}
}

// TestServiceUnknownShopDoesNotMutate 验证未知店铺不触发结算或日期刷新。
func TestServiceUnknownShopDoesNotMutate(t *testing.T) {
	for _, elapsed := range []time.Duration{25 * time.Second, 48 * time.Hour} {
		t.Run(elapsed.String(), func(t *testing.T) {
			service, store, clock := newFundedTestService(t)
			prepareTestShops(t, service)
			before, saves := service.snapshotState(), store.saveCount()
			clock.set(testTime().Add(elapsed))
			for _, id := range []string{"", "unknown", "Clothing", " coffee "} {
				for _, operation := range []func(string) (Result, error){service.Prepare, service.Upgrade} {
					result, err := operation(id)
					if !errors.Is(err, ErrUnknownShop) || !reflect.DeepEqual(result, Result{}) {
						t.Fatalf("未知店铺 %q 返回结果错误：%+v，%v", id, result, err)
					}
				}
				assertTestState(t, service.snapshotState(), before)
				assertTestStoredState(t, store, before)
				if store.saveCount() != saves {
					t.Fatal("未知店铺请求不能保存状态")
				}
			}
		})
	}
}

// TestServiceSaveFailureRollsBack 验证保存失败时资源、铺位与时间全部回滚。
func TestServiceSaveFailureRollsBack(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		run     func(service *Service, cfg Config) (Result, error)
	}{
		{"首次准备失败", 0, func(s *Service, _ Config) (Result, error) { return s.Prepare("flowers") }},
		{"结算并准备失败", 10 * time.Second, func(s *Service, _ Config) (Result, error) { return s.Prepare("flowers") }},
		{"普通结算失败", 12 * time.Second, func(s *Service, _ Config) (Result, error) { return s.Settle() }},
		{"解锁失败", 12 * time.Second, func(s *Service, cfg Config) (Result, error) { return s.Unlock(cfg.Slots[2].ID) }},
		{"升级失败", 12 * time.Second, func(s *Service, _ Config) (Result, error) { return s.Upgrade("coffee") }},
		{"跨日刷新失败", 12*time.Hour + 10*time.Second, func(s *Service, _ Config) (Result, error) { return s.Settle() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, store, clock := newFundedTestService(t)
			cfg := service.Configuration()
			prepareTestShops(t, service, "coffee")
			before, saves := service.snapshotState(), store.saveCount()
			clock.set(testTime().Add(tc.elapsed))
			injected := errors.New("注入保存失败")
			store.failSave(injected)
			result, err := tc.run(service, cfg)
			if !errors.Is(err, injected) || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("保存错误必须传回且不能返回部分结果：%+v，%v", result, err)
			}
			assertTestState(t, service.snapshotState(), before)
			assertTestStoredState(t, store, before)
			if store.saveCount() != saves+1 {
				t.Fatal("失败操作应只尝试一次保存")
			}
			store.failSave(nil)
			result, err = tc.run(service, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if result.State.Revision != before.Revision+1 || result.State.Coins < 0 {
				t.Fatalf("失败后重试不能丢失或重复收益：%+v", result.State)
			}
			assertTestStoredState(t, store, service.snapshotState())
			assertTestLedger(t, cfg, service.snapshotState())
		})
	}
}

// TestServiceCopiesAreIsolated 验证配置、快照及操作结果与内部切片隔离。
func TestServiceCopiesAreIsolated(t *testing.T) {
	cfg := fundedTestConfig(t)
	clock := &testClock{stamp: testTime()}
	store := &memoryStore{}
	service, err := NewService(cfg, store, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := service.Configuration()
	wantState := service.snapshotState()
	cfg.Shops[0].LevelCoinsPerVisitor[0] = 999
	cfg.Shops[0].ID = "mutated-input"
	cfg.Slots[2].UnlockCost = 0
	cfg.UpgradeCosts[0] = 0
	cfg.InitialCoins = 0
	returned := service.Configuration()
	returned.Shops[1].Name = "修改副本"
	returned.Shops[1].LevelCoinsPerVisitor[0] = 999
	returned.Slots[0].ID = "mutated"
	returned.UpgradeCosts[0] = 0
	returned.BaseVisitors = 1
	if !reflect.DeepEqual(service.Configuration(), wantConfig) {
		t.Fatal("输入配置或返回配置的修改污染了内部规则")
	}
	snapshot := service.Snapshot()
	snapshot.Coins = 0
	snapshot.Shops[0].Prepared = true
	snapshot.Shops[0].ID = "mutated-snapshot"
	snapshot.UnlockedSlots[0] = "mutated"
	clock.set(testTime().Add(24 * time.Hour))
	assertTestState(t, service.snapshotState(), wantState)
	clock.set(testTime())
	// 内部状态与返回值隔离：改动返回值不得影响服务。
	unlocked, err := service.Unlock(service.Configuration().Slots[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare("coffee")
	if err != nil {
		t.Fatal(err)
	}
	wantState = service.snapshotState()
	prepared.State.Shops[0].Prepared = false
	prepared.State.Coins = 0
	prepared.State.UnlockedSlots[0] = "mutated"
	unlocked.State.Spent = 0
	assertTestState(t, service.snapshotState(), wantState)
	clock.set(testTime().Add(5 * time.Second))
	settled := settleTestService(t, service)
	assertTestResult(t, settled, testUnitPrice(t, wantConfig, wantState, 0), 1, true)
	wantState = service.snapshotState()
	settled.State.Shops[0].Revenue = -1
	settled.State.Shops[0].Visitors = -1
	assertTestState(t, service.snapshotState(), wantState)
	assertTestStoredState(t, store, wantState)
}

// concurrentTestOperations 并发执行同一操作，返回成功结果与明确拒绝的次数。
// 并发竞争下「金币不足 / 已满级 / 尚未开业 / 铺位未解锁」属于允许的明确拒绝。
func concurrentTestOperations(t *testing.T, count int, operation func(int) (Result, error)) ([]Result, int) {
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
	rejected := 0
	for item := range outcomes {
		if item.err != nil {
			allowed := errors.Is(item.err, ErrInsufficientCoins) || errors.Is(item.err, ErrMaxLevel) ||
				errors.Is(item.err, ErrShopNotOpen) || errors.Is(item.err, ErrSlotLocked)
			if !allowed {
				t.Fatalf("并发操作失败：%v", item.err)
			}
			rejected++
			continue
		}
		results = append(results, item.result)
	}
	return results, rejected
}

// TestServiceConcurrentPrepareAndSettle 验证并发准备幂等且同一时间段只结算一次。
func TestServiceConcurrentPrepareAndSettle(t *testing.T) {
	service, store, clock := newFundedTestService(t)
	cfg := service.Configuration()
	// 只有一层铺位开局已解锁，因此第一轮只并发准备一层两家。
	ids := testFloorShopIDs(cfg, 1)
	results, rejected := concurrentTestOperations(t, 100, func(index int) (Result, error) {
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
	if changed != len(ids) || rejected != 0 || store.saveCount() != len(ids)+1 || service.snapshotState().Revision != int64(len(ids)+1) {
		t.Fatalf("每个店铺只能准备一次：changed=%d，rejected=%d，saves=%d", changed, rejected, store.saveCount())
	}
	// 解锁全部铺位后并发准备 + 结算，只允许一次产出。
	prepareTestShops(t, service)
	clock.set(testTime().Add(25 * time.Second))
	before := service.snapshotState()
	results, _ = concurrentTestOperations(t, 100, func(index int) (Result, error) {
		if index%2 == 0 {
			return service.Prepare(testShopIDs(cfg)[(index/2)%len(cfg.Shops)])
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
	expected, _ := testRotation(t, cfg, before, before.NextShopIndex, 5)
	state := service.snapshotState()
	if changed != 1 || coins != expected || visitors != 5 || state.VisitorsRemaining != state.DailyVisitorCap-5 {
		t.Fatalf("并发重复结算：changed=%d，coins=%d，visitors=%d，state=%+v", changed, coins, visitors, state)
	}
	assertTestStoredState(t, store, state)
	assertTestLedger(t, cfg, state)
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
				cfg.BaseVisitors = 0
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

// TestNewServiceRejectsInvalidSaves 验证存档版本、计数、铺位集合、客流上限与分段账目校验。
func TestNewServiceRejectsInvalidSaves(t *testing.T) {
	service, _, _ := newFundedTestService(t)
	cfg := service.Configuration()
	base := service.snapshotState()
	opened := copyTestState(base)
	opened.Shops[0].Prepared = true
	opened.Shops[0].Segments = []Segment{{UnitPrice: cfg.Shops[0].LevelCoinsPerVisitor[0]}}
	frozen := copyTestState(opened)
	frozen.CapFrozen, frozen.DailyVisitorCap, frozen.VisitorsRemaining = true, testCap(cfg, 1), testCap(cfg, 1)
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
		{"负金币", func(s *State) { s.Coins = -1 }},
		{"金币超安全整数", func(s *State) { s.Coins = maxSafeInteger + 1 }},
		{"负支出", func(s *State) { s.Spent = -1 }},
		{"支出超安全整数", func(s *State) { s.Spent = maxSafeInteger + 1 }},
		{"负轮询游标", func(s *State) { s.NextShopIndex = -1 }},
		{"轮询游标越界", func(s *State) { s.NextShopIndex = len(cfg.Shops) }},
		{"缺少店铺", func(s *State) { s.Shops = s.Shops[:4] }},
		{"多余店铺", func(s *State) { s.Shops = append(s.Shops, ShopState{ID: "extra", Level: 1}) }},
		{"店铺ID错误", func(s *State) { s.Shops[0].ID = "unknown" }},
		{"店铺顺序变化", func(s *State) { s.Shops[0], s.Shops[1] = s.Shops[1], s.Shops[0] }},
		{"店铺等级为零", func(s *State) { s.Shops[0].Level = 0 }},
		{"店铺等级越界", func(s *State) { s.Shops[0].Level = len(cfg.Shops[0].LevelCoinsPerVisitor) + 1 }},
		{"缺少结算时间", func(s *State) { s.LastAccrualAt = time.Time{} }},
		{"缺少观察时间", func(s *State) { s.LastObservedAt = time.Time{} }},
		{"结算时间晚于观察时间", func(s *State) { s.LastAccrualAt = s.LastObservedAt.Add(time.Nanosecond) }},
		{"业务日期无效", func(s *State) { s.BusinessDay = "not-a-date" }},
		{"业务日期与观察时间不符", func(s *State) { s.BusinessDay = "2026-09-11" }},
		{"结算时间不在业务日", func(s *State) { s.LastAccrualAt = s.LastAccrualAt.Add(-24 * time.Hour) }},
		{"未知铺位", func(s *State) { s.UnlockedSlots = append(s.UnlockedSlots, "f2-s9") }},
		{"重复铺位", func(s *State) { s.UnlockedSlots = append(s.UnlockedSlots, cfg.Slots[0].ID) }},
		{"铺位数量不足", func(s *State) { s.UnlockedSlots = s.UnlockedSlots[:1] }},
		{"跳序解锁铺位", func(s *State) { s.UnlockedSlots = append(s.UnlockedSlots, cfg.Slots[3].ID) }},
		{"缺少开局铺位", func(s *State) { s.UnlockedSlots = []string{cfg.Slots[0].ID} }},
		{"未冻结但上限非零", func(s *State) { s.DailyVisitorCap = testCap(cfg, 1) }},
		{"未冻结但剩余客流非零", func(s *State) { s.VisitorsRemaining = 1 }},
		{"冻结但上限为零", func(s *State) { s.CapFrozen = true }},
		{"冻结档位低于下界", func(s *State) {
			s.CapFrozen, s.DailyVisitorCap, s.VisitorsRemaining = true, cfg.BaseVisitors, cfg.BaseVisitors
		}},
		{"冻结档位高于上界", func(s *State) {
			capacity := testCap(cfg, int64(len(cfg.Shops))+1)
			s.CapFrozen, s.DailyVisitorCap, s.VisitorsRemaining = true, capacity, capacity
		}},
		{"冻结档位不落档", func(s *State) {
			capacity := cfg.BaseVisitors + cfg.VisitorsPerShop + 1
			s.CapFrozen, s.DailyVisitorCap, s.VisitorsRemaining = true, capacity, capacity
		}},
		{"冻结但无开业店铺", func(s *State) {
			capacity := testCap(cfg, 1)
			s.CapFrozen, s.DailyVisitorCap, s.VisitorsRemaining = true, capacity, capacity
		}},
		{"剩余客流超过上限", func(s *State) {
			capacity := testCap(cfg, 1)
			s.CapFrozen, s.DailyVisitorCap, s.VisitorsRemaining = true, capacity, capacity+1
		}},
		{"未开业店铺存在账目", func(s *State) {
			s.Shops[0].Visitors, s.Shops[0].Revenue = 1, cfg.Shops[0].LevelCoinsPerVisitor[0]
			s.Coins += s.Shops[0].Revenue
		}},
		{"未开业店铺存在空段", func(s *State) { s.Shops[0].Segments = []Segment{{UnitPrice: cfg.Shops[0].LevelCoinsPerVisitor[0]}} }},
		{"开业店铺缺少分段", func(s *State) { s.Shops[0].Prepared = true }},
		{"未解锁铺位已开业", func(s *State) {
			s.Shops[2].Prepared = true
			s.Shops[2].Segments = []Segment{{UnitPrice: cfg.Shops[2].LevelCoinsPerVisitor[0]}}
		}},
		{"分段单价为零", func(s *State) {
			s.Shops[0].Prepared = true
			s.Shops[0].Segments = []Segment{{}}
		}},
		{"分段账目不符", func(s *State) {
			s.Shops[0].Prepared = true
			s.Shops[0].Segments = []Segment{{UnitPrice: cfg.Shops[0].LevelCoinsPerVisitor[0], Visitors: 1}}
		}},
		{"分段聚合不符", func(s *State) {
			s.Shops[0].Prepared = true
			price := cfg.Shops[0].LevelCoinsPerVisitor[0]
			s.Shops[0].Segments = []Segment{{UnitPrice: price, Visitors: 1, Revenue: price}}
		}},
		{"负累计客流", func(s *State) {
			s.Shops[0].Prepared = true
			s.Shops[0].Segments = []Segment{{UnitPrice: cfg.Shops[0].LevelCoinsPerVisitor[0], Visitors: -1}}
		}},
		{"分段收益超安全整数", func(s *State) {
			s.Shops[0].Prepared = true
			s.Shops[0].Segments = []Segment{{UnitPrice: 1, Visitors: maxSafeInteger, Revenue: maxSafeInteger}}
		}},
		{"金币与累计账目不匹配", func(s *State) { s.Coins++ }},
		{"支出与金币恒等式不匹配", func(s *State) { s.Spent++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := copyTestState(base)
			tc.mutate(&state)
			original := copyTestState(state)
			store := &memoryStore{state: state, exists: true}
			loaded, err := NewService(cfg, store, testTime)
			if err == nil || loaded != nil {
				t.Fatalf("无效存档应被拒绝：%+v，%v", state, err)
			}
			if store.saveCount() != 0 {
				t.Fatal("无效存档不能被默认状态覆盖")
			}
			assertTestStoredState(t, store, original)
		})
	}
	// 合法的未冻结与已冻结存档必须可加载。
	for name, valid := range map[string]State{"未冻结": opened, "已冻结": frozen} {
		t.Run("合法存档/"+name, func(t *testing.T) {
			store := &memoryStore{state: copyTestState(valid), exists: true}
			if _, err := NewService(cfg, store, testTime); err != nil {
				t.Fatalf("合法存档应可加载：%+v，%v", valid, err)
			}
		})
	}
}

// TestNewServiceNormalizesUnlockedSlotOrder 验证铺位顺序不影响加载，加载后规范化为升序。
func TestNewServiceNormalizesUnlockedSlotOrder(t *testing.T) {
	service, _, _ := newFundedTestService(t)
	cfg := service.Configuration()
	state := service.snapshotState()
	reversed := make([]string, 0, len(state.UnlockedSlots))
	for i := len(state.UnlockedSlots) - 1; i >= 0; i-- {
		reversed = append(reversed, state.UnlockedSlots[i])
	}
	state.UnlockedSlots = reversed
	store := &memoryStore{state: copyTestState(state), exists: true}
	loaded, err := NewService(cfg, store, testTime)
	if err != nil {
		t.Fatalf("铺位顺序不同不应拒绝加载：%v", err)
	}
	if !reflect.DeepEqual(loaded.snapshotState().UnlockedSlots, service.snapshotState().UnlockedSlots) {
		t.Fatalf("加载后应规范化为解锁顺序：%v", loaded.snapshotState().UnlockedSlots)
	}
	if store.saveCount() != 0 {
		t.Fatal("加载不应改写存档文件")
	}
}

// TestServiceNumericLimitRollsBack 验证安全整数上限错误不发布部分结算。
func TestServiceNumericLimitRollsBack(t *testing.T) {
	for _, limit := range []string{"修订号上限", "结算中途金币上限"} {
		t.Run(limit, func(t *testing.T) {
			service, _, clock := newTestService(t)
			cfg := testConfig(t)
			index := testShopIndex(t, cfg, "coffee")
			price := cfg.Shops[index].LevelCoinsPerVisitor[0]
			state := service.snapshotState()
			state.Shops[index].Prepared = true
			state.Shops[index].Segments = []Segment{{UnitPrice: price}}
			state.CapFrozen = true
			state.DailyVisitorCap = testCap(cfg, 1)
			state.VisitorsRemaining = state.DailyVisitorCap
			if limit == "修订号上限" {
				state.Revision = maxSafeInteger
			} else {
				units := (maxSafeInteger - cfg.InitialCoins) / price
				revenue := units * price
				state.Shops[index].Visitors, state.Shops[index].Revenue = units, revenue
				state.Shops[index].Segments = []Segment{{UnitPrice: price, Visitors: units, Revenue: revenue}}
				state.Coins = cfg.InitialCoins + revenue
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
			assertTestState(t, service.snapshotState(), state)
			assertTestStoredState(t, store, state)
			if store.saveCount() != 0 {
				t.Fatal("超限结算不能写入部分结果")
			}
		})
	}
}

// TestSpentCannotOverflow 验证金币恒等式使「累计支出溢出」不可达：合法存档下
// spent + 本次花费 <= initialCoins + Σrevenue <= maxSafeInteger，故无需额外护栏。
func TestSpentCannotOverflow(t *testing.T) {
	service, _, _ := newFundedTestService(t)
	cfg := service.Configuration()
	prepareTestShops(t, service, "coffee")
	// 构造一份合法存档：收益与支出同时接近安全整数上界，余额仍够支付下一次升级。
	state := service.snapshotState()
	revenue := maxSafeInteger - cfg.InitialCoins - 1
	state.Shops[0].Visitors, state.Shops[0].Revenue = revenue, revenue
	state.Shops[0].Segments = []Segment{{UnitPrice: 1, Visitors: revenue, Revenue: revenue}}
	state.Spent = cfg.InitialCoins + revenue - cfg.UpgradeCosts[0]
	state.Coins = cfg.UpgradeCosts[0]
	store := &memoryStore{state: copyTestState(state), exists: true}
	service, err := NewService(cfg, store, testTime)
	if err != nil {
		t.Fatalf("合法上限存档应可加载：%v", err)
	}
	before := service.snapshotState()
	if before.Coins != cfg.UpgradeCosts[0] || before.Spent+cfg.UpgradeCosts[0] > maxSafeInteger {
		t.Fatalf("测试前提不成立：%+v", before)
	}
	if _, err := service.Upgrade("coffee"); err != nil {
		t.Fatalf("该存档仍可完成一次升级：%v", err)
	}
	after := service.snapshotState()
	if after.Coins != 0 || after.Spent != before.Spent+cfg.UpgradeCosts[0] || after.Spent > maxSafeInteger {
		t.Fatalf("升级后支出或余额错误：%+v", after)
	}
	assertTestLedger(t, cfg, after)
}
