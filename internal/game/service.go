package game

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"
)

// Domain errors are mapped to stable HTTP error codes by the API layer.
var (
	ErrUnknownShop       = errors.New("unknown shop")
	ErrUnknownSlot       = errors.New("unknown slot")
	ErrSlotLocked        = errors.New("slot is not unlocked yet")
	ErrSlotOrder         = errors.New("slots must be unlocked in order")
	ErrShopNotOpen       = errors.New("shop is not open yet")
	ErrMaxLevel          = errors.New("shop already reached the maximum level")
	ErrInsufficientCoins = errors.New("not enough coins")
	ErrClockBackwards    = errors.New("server clock moved backwards")
	ErrNumericLimit      = errors.New("safe integer limit reached")
)

// schemaVersion identifies the save layout. v2 brought the segmented ledger, unlocked
// slots, per-shop levels and the two-state visitor cap; v3 adds todayEarned.
// TodayEarned cannot be rebuilt from an older save: segments record no timestamps, so a
// single business day's slice is unrecoverable (架构现状 §8-9). Defaulting it to zero would
// let the client show a plausible-but-wrong "today's income", which is worse than the
// development-period save reset this bump forces.
const schemaVersion = 3

const maxSafeInteger int64 = 1<<53 - 1

// Segment records one price interval of a shop's ledger. A segment is appended
// whenever the shop's unit price changes, even when it has no visitors yet.
type Segment struct {
	UnitPrice int64 `json:"unitPrice"`
	Visitors  int64 `json:"visitors"`
	Revenue   int64 `json:"revenue"`
}

// ShopState records cumulative server-calculated income, level and preparation status.
type ShopState struct {
	ID       string    `json:"id"`
	Prepared bool      `json:"prepared"`
	Level    int       `json:"level"`
	Visitors int64     `json:"visitors"`
	Revenue  int64     `json:"revenue"`
	Segments []Segment `json:"segments"`
}

// State is the versioned save for the single local development account.
// capFrozen is the only judge of whether dailyVisitorCap is meaningful: an
// unfrozen day stores the placeholder 0 and every reader ignores it.
// TodayEarned counts arrival income booked inside the current business day only:
// spending never reduces it, and it resets when the day rolls over.
type State struct {
	SchemaVersion     int         `json:"schemaVersion"`
	RulesFingerprint  string      `json:"rulesFingerprint"`
	Revision          int64       `json:"revision"`
	Coins             int64       `json:"coins"`
	Spent             int64       `json:"spent"`
	TodayEarned       int64       `json:"todayEarned"`
	BusinessDay       string      `json:"businessDay"`
	CapFrozen         bool        `json:"capFrozen"`
	DailyVisitorCap   int64       `json:"dailyVisitorCap"`
	VisitorsRemaining int64       `json:"visitorsRemaining"`
	LastAccrualAt     time.Time   `json:"lastAccrualAt"`
	LastObservedAt    time.Time   `json:"lastObservedAt"`
	NextShopIndex     int         `json:"nextShopIndex"`
	UnlockedSlots     []string    `json:"unlockedSlots"`
	Shops             []ShopState `json:"shops"`
}

// ShopView is the client-facing shop snapshot; unitPrice and upgradeCost are derived.
//
// Reading UnitPrice: it is this shop's CURRENT per-visitor tier, NOT the price the
// visitors of one settlement round actually paid. `mutate` accrues before it applies the
// command, so a Prepare or Upgrade response can carry a raised UnitPrice while the very
// same round's visitors were booked at the previous, lower tier. A presentation layer
// that shows an amount a visitor paid must take the booked price from the previous
// snapshot (see `Arrival.unitPrice` client-side), not from this field.
type ShopView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Floor       int    `json:"floor"`
	Slot        int    `json:"slot"`
	Prepared    bool   `json:"prepared"`
	Level       int    `json:"level"`
	UnitPrice   int64  `json:"unitPrice"`
	Visitors    int64  `json:"visitors"`
	Revenue     int64  `json:"revenue"`
	UpgradeCost *int64 `json:"upgradeCost"`
	// UnitPriceBase and FloorBonus split UnitPrice into the configured level price and
	// the full-floor addition actually applied to this shop, so that
	// UnitPriceBase+FloorBonus == UnitPrice. The split is published because the design
	// shows the composition to the player while GDD §6.2 forbids the client from
	// deciding floor fullness on its own.
	UnitPriceBase int64 `json:"unitPriceBase"`
	FloorBonus    int64 `json:"floorBonus"`
	// NextUnitPrice is the per-visitor price after one upgrade, nil at max level.
	// Floor fullness depends only on unlocked and prepared slots, never on level,
	// so the same FloorBonus applies after the upgrade.
	NextUnitPrice *int64 `json:"nextUnitPrice"`
}

// MallView is the client-facing snapshot. DailyVisitorCap is nil exactly when the
// day is not frozen, so the client never sees the placeholder 0. TodayEarned is the
// arrival income of the current business day, so the client never subtracts a previous
// day's coins to guess it.
type MallView struct {
	SchemaVersion     int        `json:"schemaVersion"`
	RulesFingerprint  string     `json:"rulesFingerprint"`
	Revision          int64      `json:"revision"`
	Coins             int64      `json:"coins"`
	Spent             int64      `json:"spent"`
	TodayEarned       int64      `json:"todayEarned"`
	BusinessDay       string     `json:"businessDay"`
	CapFrozen         bool       `json:"capFrozen"`
	DailyVisitorCap   *int64     `json:"dailyVisitorCap"`
	VisitorsServed    int64      `json:"visitorsServed"`
	VisitorsRemaining int64      `json:"visitorsRemaining"`
	LastAccrualAt     time.Time  `json:"lastAccrualAt"`
	LastObservedAt    time.Time  `json:"lastObservedAt"`
	UnlockedSlots     []string   `json:"unlockedSlots"`
	NextSlotID        *string    `json:"nextSlotId"`
	NextUnlockCost    *int64     `json:"nextUnlockCost"`
	Shops             []ShopView `json:"shops"`
}

// Result describes one atomic operation, including newly served visitors and coins.
type Result struct {
	State        MallView `json:"state"`
	EarnedCoins  int64    `json:"earnedCoins"`
	VisitorsUsed int64    `json:"visitorsUsed"`
	Changed      bool     `json:"changed"`
}

// Service serializes mutations and commits the save before publishing a new state.
// Exactly one Service process may own a save file.
type Service struct {
	mu          sync.Mutex
	cfg         Config
	zone        *time.Location
	store       Store
	now         func() time.Time
	state       State
	maxLevel    int
	slotIndex   map[string]int // slot id -> index in cfg.Slots
	shopSlot    []int          // shop index -> index in cfg.Slots
	slotShop    []int          // index in cfg.Slots -> shop index
	floorSlots  map[int][]int  // floor -> slot indices
	floorShops  map[int][]int  // floor -> shop indices
	initialOpen []string       // slots unlocked for a brand new account
}

// NewService opens a valid save or creates one only when no save exists.
func NewService(cfg Config, store Store, now func() time.Time) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || now == nil {
		return nil, fmt.Errorf("store and server clock are required")
	}
	cfg.Shops = append([]ShopConfig(nil), cfg.Shops...)
	for i := range cfg.Shops {
		cfg.Shops[i].LevelCoinsPerVisitor = append([]int64(nil), cfg.Shops[i].LevelCoinsPerVisitor...)
	}
	cfg.Slots = append([]SlotConfig(nil), cfg.Slots...)
	cfg.UpgradeCosts = append([]int64(nil), cfg.UpgradeCosts...)
	zone, err := time.LoadLocation(cfg.BusinessTimezone)
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, store: store, zone: zone, now: now, maxLevel: len(cfg.Shops[0].LevelCoinsPerVisitor)}
	s.buildLayout()
	state, err := store.Load()
	if errors.Is(err, os.ErrNotExist) {
		stamp := now().UTC()
		state = State{
			SchemaVersion: schemaVersion, RulesFingerprint: fingerprint(cfg), Revision: 1,
			Coins: cfg.InitialCoins, BusinessDay: stamp.In(zone).Format(time.DateOnly),
			LastAccrualAt: stamp, LastObservedAt: stamp,
			UnlockedSlots: append([]string(nil), s.initialOpen...),
		}
		for _, shop := range cfg.Shops {
			state.Shops = append(state.Shops, ShopState{ID: shop.ID, Level: 1})
		}
		if err := store.Save(state); err != nil {
			return nil, fmt.Errorf("create save: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("load save: %w", err)
	}
	if err := s.validateState(state); err != nil {
		return nil, fmt.Errorf("invalid save; original left untouched: %w", err)
	}
	// Element order carries no meaning; normalize it in memory without rewriting the file.
	state.UnlockedSlots = s.canonicalSlots(state.UnlockedSlots)
	s.state = clone(state)
	return s, nil
}

// buildLayout derives every slot/shop relation once from the validated rules.
func (s *Service) buildLayout() {
	s.slotIndex = make(map[string]int, len(s.cfg.Slots))
	s.floorSlots = make(map[int][]int, 2)
	for i, slot := range s.cfg.Slots {
		s.slotIndex[slot.ID] = i
		s.floorSlots[slot.Floor] = append(s.floorSlots[slot.Floor], i)
	}
	s.shopSlot = make([]int, len(s.cfg.Shops))
	s.slotShop = make([]int, len(s.cfg.Slots))
	s.floorShops = make(map[int][]int, 2)
	for i, shop := range s.cfg.Shops {
		s.shopSlot[i] = s.slotIndex[s.slotIDOfShop(shop)]
		s.slotShop[s.shopSlot[i]] = i
		s.floorShops[shop.Floor] = append(s.floorShops[shop.Floor], i)
	}
	s.initialOpen = s.canonicalSlots(s.initialSlots())
}

// slotIDOfShop returns the slot that is bound to one shop by floor and position.
func (s *Service) slotIDOfShop(shop ShopConfig) string {
	for _, slot := range s.cfg.Slots {
		if slot.Floor == shop.Floor && slot.Position == shop.Slot {
			return slot.ID
		}
	}
	return ""
}

func (s *Service) initialSlots() []string {
	ids := make([]string, 0, len(s.cfg.Slots))
	for _, slot := range s.cfg.Slots {
		if slot.UnlockOrder == 0 {
			ids = append(ids, slot.ID)
		}
	}
	return ids
}

// canonicalSlots orders slot ids by unlock order, then by configuration order.
// Passing nil returns every configured slot id in that order.
func (s *Service) canonicalSlots(ids []string) []string {
	selected := ids
	if selected == nil {
		selected = make([]string, 0, len(s.cfg.Slots))
		for _, slot := range s.cfg.Slots {
			selected = append(selected, slot.ID)
		}
	}
	ordered := append([]string(nil), selected...)
	sort.Slice(ordered, func(a, b int) bool {
		left, right := s.cfg.Slots[s.slotIndex[ordered[a]]], s.cfg.Slots[s.slotIndex[ordered[b]]]
		if left.UnlockOrder != right.UnlockOrder {
			return left.UnlockOrder < right.UnlockOrder
		}
		return s.slotIndex[ordered[a]] < s.slotIndex[ordered[b]]
	})
	return ordered
}

// Configuration returns an isolated copy of the active prototype rules.
func (s *Service) Configuration() Config {
	cfg := s.cfg
	cfg.Shops = make([]ShopConfig, len(s.cfg.Shops))
	for i, shop := range s.cfg.Shops {
		shop.LevelCoinsPerVisitor = append([]int64(nil), shop.LevelCoinsPerVisitor...)
		cfg.Shops[i] = shop
	}
	cfg.Slots = append([]SlotConfig(nil), s.cfg.Slots...)
	cfg.UpgradeCosts = append([]int64(nil), s.cfg.UpgradeCosts...)
	return cfg
}

// Snapshot returns the last committed state without settling income or refreshing a day.
func (s *Service) Snapshot() MallView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(s.state)
}

// Prepare opens one shop after settling the interval before that shop was prepared.
// Repeating preparation for an already open shop has no effect.
func (s *Service) Prepare(id string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.shopIndex(id)
	if index < 0 {
		return Result{}, ErrUnknownShop
	}
	if s.state.Shops[index].Prepared {
		return Result{State: s.view(s.state)}, nil
	}
	if !s.unlocked(s.state, s.cfg.Slots[s.shopSlot[index]].ID) {
		return Result{}, ErrSlotLocked
	}
	return s.mutate(func(next *State, now time.Time) error {
		// Hard order: mark prepared, then recompute the floor, then record the price.
		next.Shops[index].Prepared = true
		// Idle settlements deliberately leave the settlement cursor untouched, so when
		// the very first shop of a day opens the cursor is stale and must be anchored
		// to this instant; later shops keep the remainder that accrual already preserved.
		if openShopCount(*next) == 1 {
			next.LastAccrualAt = now
		}
		s.syncFloorSegments(next, s.cfg.Shops[index].Floor)
		return nil
	})
}

// Unlock buys one slot after settling income that was already earned.
// Repeating the unlock of an owned slot is an idempotent success.
func (s *Service) Unlock(slotID string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.slotIndex[slotID]
	if !ok {
		return Result{}, ErrUnknownSlot
	}
	if s.unlocked(s.state, slotID) {
		return Result{State: s.view(s.state)}, nil
	}
	slot := s.cfg.Slots[index]
	if !s.lowerOrdersUnlocked(s.state, slot) {
		return Result{}, ErrSlotOrder
	}
	if s.state.Coins < slot.UnlockCost {
		return Result{}, ErrInsufficientCoins
	}
	return s.mutate(func(next *State, _ time.Time) error {
		// No overflow guard is needed: a valid save keeps
		// coins == initialCoins + Σrevenue − spent, so coins >= cost implies
		// spent + cost <= total <= maxSafeInteger.
		next.Coins -= slot.UnlockCost
		next.Spent += slot.UnlockCost
		next.UnlockedSlots = s.canonicalSlots(append(next.UnlockedSlots, slotID))
		return nil
	})
}

// Upgrade raises one open shop to its next level and starts a new ledger segment.
func (s *Service) Upgrade(shopID string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.shopIndex(shopID)
	if index < 0 {
		return Result{}, ErrUnknownShop
	}
	shop := s.state.Shops[index]
	// Report "unlock first" and "open first" separately: they are different fixes for the player.
	if !s.unlocked(s.state, s.cfg.Slots[s.shopSlot[index]].ID) {
		return Result{}, ErrSlotLocked
	}
	if !shop.Prepared {
		return Result{}, ErrShopNotOpen
	}
	if shop.Level >= s.maxLevel {
		return Result{}, ErrMaxLevel
	}
	cost := s.cfg.UpgradeCosts[shop.Level-1]
	if s.state.Coins < cost {
		return Result{}, ErrInsufficientCoins
	}
	return s.mutate(func(next *State, _ time.Time) error {
		// See Unlock: the coin identity makes an overflowing spent value impossible.
		next.Coins -= cost
		next.Spent += cost
		next.Shops[index].Level++
		s.syncSegments(next, index)
		return nil
	})
}

// Settle awards only configured income using the server clock and available visitors.
func (s *Service) Settle() (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(nil)
}

func (s *Service) shopIndex(id string) int {
	for i, shop := range s.cfg.Shops {
		if shop.ID == id {
			return i
		}
	}
	return -1
}

func (s *Service) unlocked(state State, slotID string) bool {
	for _, id := range state.UnlockedSlots {
		if id == slotID {
			return true
		}
	}
	return false
}

func (s *Service) lowerOrdersUnlocked(state State, slot SlotConfig) bool {
	for _, other := range s.cfg.Slots {
		if other.UnlockOrder < slot.UnlockOrder && !s.unlocked(state, other.ID) {
			return false
		}
	}
	return true
}

// floorFull reports whether every slot on one floor is unlocked and open.
func (s *Service) floorFull(state State, floor int) bool {
	for _, slotIndex := range s.floorSlots[floor] {
		if !s.unlocked(state, s.cfg.Slots[slotIndex].ID) || !state.Shops[s.slotShop[slotIndex]].Prepared {
			return false
		}
	}
	return true
}

// levelPrice is the configured per-visitor price of a shop at its current level.
func (s *Service) levelPrice(state State, shopIndex int) int64 {
	return s.cfg.Shops[shopIndex].LevelCoinsPerVisitor[state.Shops[shopIndex].Level-1]
}

// floorBonus is the full-floor addition actually applied to one shop, zero while that
// floor is not full. It never depends on level, so it survives an upgrade unchanged.
func (s *Service) floorBonus(state State, shopIndex int) int64 {
	if s.floorFull(state, s.cfg.Shops[shopIndex].Floor) {
		return s.cfg.FullFloorBonus
	}
	return 0
}

// unitPrice is the configured level price plus the full-floor bonus of that floor.
func (s *Service) unitPrice(state State, shopIndex int) int64 {
	return s.levelPrice(state, shopIndex) + s.floorBonus(state, shopIndex)
}

// maxSegments bounds one shop's ledger: the opening segment, one segment per
// configured upgrade price change, and one full-floor change (GDD §5.6). It is
// derived from the config so a rule change moves the bound instead of freezing
// today's numbers into code.
func (s *Service) maxSegments() int {
	return 1 + len(s.cfg.UpgradeCosts) + 1
}

// syncFloorSegments appends a segment for every open shop on one floor whose price changed.
func (s *Service) syncFloorSegments(state *State, floor int) {
	for _, shopIndex := range s.floorShops[floor] {
		if state.Shops[shopIndex].Prepared {
			s.syncSegments(state, shopIndex)
		}
	}
}

// syncSegments appends one segment when the shop has none or its price changed.
// A price change always appends, even when the previous segment has no visitors.
func (s *Service) syncSegments(state *State, shopIndex int) {
	price := s.unitPrice(*state, shopIndex)
	segments := state.Shops[shopIndex].Segments
	if len(segments) > 0 && segments[len(segments)-1].UnitPrice == price {
		return
	}
	state.Shops[shopIndex].Segments = append(segments, Segment{UnitPrice: price})
}

// mutate settles first, then applies one command, then commits before publishing.
// A nil apply is a plain settlement; when it moves nothing but the observation
// cursor it is reported as unchanged and never committed.
func (s *Service) mutate(apply func(*State, time.Time) error) (Result, error) {
	now := s.now().UTC()
	if now.Before(s.state.LastObservedAt) {
		return Result{}, ErrClockBackwards
	}
	next := clone(s.state)
	result := Result{}
	if err := s.accrue(&next, now, &result); err != nil {
		return Result{}, err
	}
	if apply != nil {
		if err := apply(&next, now); err != nil {
			return Result{}, err
		}
	}
	next.LastObservedAt = now
	if apply == nil && sameCommittedState(next, s.state) {
		return Result{State: s.view(s.state)}, nil
	}
	if next.Revision == maxSafeInteger {
		return Result{}, ErrNumericLimit
	}
	next.Revision++
	if err := s.store.Save(next); err != nil {
		return Result{}, fmt.Errorf("commit save: %w", err)
	}
	s.state = next
	result.State, result.Changed = s.view(next), true
	return result, nil
}

// sameCommittedState reports whether two states differ only by the observation
// cursor, which is the one field a settlement may move without committing.
func sameCommittedState(a, b State) bool {
	left, right := a, b
	left.LastObservedAt, right.LastObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

// openShopCount is the number of shops currently open, which drives both the
// daily visitor cap tier and whether a settlement can produce any visitor.
func openShopCount(state State) int64 {
	var count int64
	for _, shop := range state.Shops {
		if shop.Prepared {
			count++
		}
	}
	return count
}

func (s *Service) accrue(state *State, now time.Time, result *Result) error {
	local := now.In(s.zone)
	day := local.Format(time.DateOnly)
	if state.BusinessDay != day {
		state.BusinessDay = day
		state.CapFrozen = false
		state.DailyVisitorCap = 0
		state.VisitorsRemaining = 0
		state.TodayEarned = 0
		// Past business days are not backfilled; no multi-day offline rewards.
		state.LastAccrualAt = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.zone).UTC()
	}
	openShops := openShopCount(*state)
	// Nobody can be served before a shop opens, so the cap stays unfrozen. An idle
	// poll must not move the settlement cursor, or it would look like a real change.
	if openShops == 0 || (state.CapFrozen && state.VisitorsRemaining == 0) {
		return nil
	}
	interval := time.Duration(s.cfg.VisitorIntervalSeconds) * time.Second
	elapsed := now.Sub(state.LastAccrualAt)
	visits := int64(elapsed / interval)
	if !state.CapFrozen {
		if visits == 0 {
			state.LastAccrualAt = now.Add(-(elapsed % interval))
			return nil
		}
		// Freeze inside the same commit as the first served visitor.
		state.CapFrozen = true
		state.DailyVisitorCap = s.cfg.BaseVisitors + s.cfg.VisitorsPerShop*openShops
		state.VisitorsRemaining = state.DailyVisitorCap
	}
	visits = min(visits, state.VisitorsRemaining)
	state.LastAccrualAt = now.Add(-(elapsed % interval))
	for range visits {
		for !state.Shops[state.NextShopIndex].Prepared {
			state.NextShopIndex = (state.NextShopIndex + 1) % len(state.Shops)
		}
		index := state.NextShopIndex
		shop := &state.Shops[index]
		segments := shop.Segments
		if len(segments) == 0 {
			return fmt.Errorf("shop %s is open without a ledger segment", shop.ID)
		}
		last := &segments[len(segments)-1]
		amount := s.unitPrice(*state, index)
		if state.Coins > maxSafeInteger-amount || shop.Visitors == maxSafeInteger ||
			shop.Revenue > maxSafeInteger-amount || last.Visitors == maxSafeInteger ||
			last.Revenue > maxSafeInteger-amount {
			return ErrNumericLimit
		}
		state.Coins += amount
		state.TodayEarned += amount
		shop.Visitors++
		shop.Revenue += amount
		last.Visitors++
		last.Revenue += amount
		state.VisitorsRemaining--
		result.EarnedCoins += amount
		result.VisitorsUsed++
		state.NextShopIndex = (index + 1) % len(state.Shops)
	}
	return nil
}

func (s *Service) validateState(state State) error {
	if state.SchemaVersion != schemaVersion || state.RulesFingerprint != fingerprint(s.cfg) {
		return fmt.Errorf("schema or rules changed; use a separate development save")
	}
	if state.Revision < 1 || state.Revision > maxSafeInteger ||
		state.Coins < 0 || state.Coins > maxSafeInteger ||
		state.Spent < 0 || state.Spent > maxSafeInteger ||
		state.TodayEarned < 0 || state.TodayEarned > maxSafeInteger ||
		state.NextShopIndex < 0 || state.NextShopIndex >= len(s.cfg.Shops) ||
		len(state.Shops) != len(s.cfg.Shops) {
		return fmt.Errorf("state counters out of range")
	}
	if state.LastAccrualAt.IsZero() || state.LastObservedAt.IsZero() || state.LastAccrualAt.After(state.LastObservedAt) ||
		state.BusinessDay != state.LastObservedAt.In(s.zone).Format(time.DateOnly) ||
		state.BusinessDay != state.LastAccrualAt.In(s.zone).Format(time.DateOnly) {
		return fmt.Errorf("invalid settlement timestamps")
	}
	unlocked, err := s.validateUnlockedSlots(state.UnlockedSlots)
	if err != nil {
		return err
	}
	if err := s.validateVisitorCap(state); err != nil {
		return err
	}
	total := s.cfg.InitialCoins
	for i, shop := range state.Shops {
		if shop.ID != s.cfg.Shops[i].ID || shop.Level < 1 || shop.Level > s.maxLevel {
			return fmt.Errorf("invalid shop state: %s", shop.ID)
		}
		if !unlocked[s.cfg.Slots[s.shopSlot[i]].ID] && shop.Prepared {
			return fmt.Errorf("shop %s is open on a locked slot", shop.ID)
		}
		if !shop.Prepared && (shop.Visitors != 0 || shop.Revenue != 0 || len(shop.Segments) != 0) {
			return fmt.Errorf("shop %s is closed but keeps a ledger", shop.ID)
		}
		if shop.Prepared && len(shop.Segments) == 0 {
			return fmt.Errorf("shop %s is open without a ledger segment", shop.ID)
		}
		// The ledger only grows at three points: opening the shop, one segment per
		// configured upgrade price change, and the floor's full state flipping once
		// (GDD §5.6). Local single-account saves never exceed it by construction, but
		// M07 started receiving externally maintained saves, where a self-consistent
		// ledger with any segment count would otherwise pass every identity check.
		if len(shop.Segments) > s.maxSegments() {
			return fmt.Errorf("shop %s has %d ledger segments, expected at most %d", shop.ID, len(shop.Segments), s.maxSegments())
		}
		var visitors, revenue int64
		for _, segment := range shop.Segments {
			if segment.UnitPrice < 1 || segment.Visitors < 0 || segment.Visitors > maxSafeInteger/segment.UnitPrice ||
				segment.Revenue != segment.Visitors*segment.UnitPrice || segment.Revenue > maxSafeInteger ||
				visitors > maxSafeInteger-segment.Visitors || revenue > maxSafeInteger-segment.Revenue {
				return fmt.Errorf("invalid ledger segment: %s", shop.ID)
			}
			visitors += segment.Visitors
			revenue += segment.Revenue
		}
		if shop.Visitors != visitors || shop.Revenue != revenue || total > maxSafeInteger-shop.Revenue {
			return fmt.Errorf("invalid shop accounting: %s", shop.ID)
		}
		total += shop.Revenue
	}
	if total < state.Spent || state.Coins != total-state.Spent {
		return fmt.Errorf("coin total does not match revenue and spending")
	}
	// TodayEarned is one business day's slice of that lifetime total, so it can never
	// exceed it. Spending is booked through Spent and never lowers this field.
	if state.TodayEarned > total {
		return fmt.Errorf("today income exceeds the lifetime revenue")
	}
	return nil
}

// validateUnlockedSlots enforces the serialized structure: known ids, no duplicates,
// at least the opening slots, and a downward-closed unlock order (no skipping).
func (s *Service) validateUnlockedSlots(ids []string) (map[string]bool, error) {
	unlocked := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, ok := s.slotIndex[id]; !ok {
			return nil, fmt.Errorf("save contains an unknown slot: %q", id)
		}
		if unlocked[id] {
			return nil, fmt.Errorf("save contains a duplicate slot: %q", id)
		}
		unlocked[id] = true
	}
	if len(unlocked) < len(s.initialOpen) || len(unlocked) > len(s.cfg.Slots) {
		return nil, fmt.Errorf("save unlocks %d slots, expected %d..%d", len(unlocked), len(s.initialOpen), len(s.cfg.Slots))
	}
	for _, id := range s.initialOpen {
		if !unlocked[id] {
			return nil, fmt.Errorf("save is missing the opening slot: %q", id)
		}
	}
	for _, id := range ids {
		if !s.lowerOrdersUnlocked(State{UnlockedSlots: ids}, s.cfg.Slots[s.slotIndex[id]]) {
			return nil, fmt.Errorf("save skips an unlock order before %q", id)
		}
	}
	return unlocked, nil
}

// validateVisitorCap enforces the two-state rule. The frozen value can no longer be
// recomputed from the current state, so it only has to be one of the configured tiers.
func (s *Service) validateVisitorCap(state State) error {
	if !state.CapFrozen {
		if state.DailyVisitorCap != 0 || state.VisitorsRemaining != 0 {
			return fmt.Errorf("an unfrozen visitor cap must keep the zero placeholder")
		}
		return nil
	}
	base, per := s.cfg.BaseVisitors, s.cfg.VisitorsPerShop
	if state.DailyVisitorCap < base+per || state.DailyVisitorCap > base+per*int64(len(s.cfg.Shops)) ||
		(state.DailyVisitorCap-base)%per != 0 {
		return fmt.Errorf("frozen visitor cap is outside the configured tiers")
	}
	if state.VisitorsRemaining < 0 || state.VisitorsRemaining > state.DailyVisitorCap {
		return fmt.Errorf("remaining visitors out of range")
	}
	open := false
	for _, shop := range state.Shops {
		open = open || shop.Prepared
	}
	if !open {
		return fmt.Errorf("a frozen visitor cap requires an open shop")
	}
	return nil
}

// view derives the client snapshot. dailyVisitorCap stays nil while the day is unfrozen.
func (s *Service) view(state State) MallView {
	view := MallView{
		SchemaVersion: state.SchemaVersion, RulesFingerprint: state.RulesFingerprint, Revision: state.Revision,
		Coins: state.Coins, Spent: state.Spent, TodayEarned: state.TodayEarned,
		BusinessDay: state.BusinessDay, CapFrozen: state.CapFrozen,
		VisitorsRemaining: state.VisitorsRemaining, LastAccrualAt: state.LastAccrualAt,
		LastObservedAt: state.LastObservedAt, UnlockedSlots: append([]string(nil), state.UnlockedSlots...),
	}
	if state.CapFrozen {
		frozen := state.DailyVisitorCap
		view.DailyVisitorCap = &frozen
		view.VisitorsServed = frozen - state.VisitorsRemaining
	}
	if slot, ok := s.nextSlot(state); ok {
		view.NextSlotID, view.NextUnlockCost = &slot.ID, &slot.UnlockCost
	}
	for i, shop := range state.Shops {
		item := ShopView{
			ID: shop.ID, Name: s.cfg.Shops[i].Name, Floor: s.cfg.Shops[i].Floor, Slot: s.cfg.Shops[i].Slot,
			Prepared: shop.Prepared, Level: shop.Level, UnitPrice: s.unitPrice(state, i),
			Visitors: shop.Visitors, Revenue: shop.Revenue,
			UnitPriceBase: s.levelPrice(state, i), FloorBonus: s.floorBonus(state, i),
		}
		if shop.Level < s.maxLevel {
			cost := s.cfg.UpgradeCosts[shop.Level-1]
			next := s.cfg.Shops[i].LevelCoinsPerVisitor[shop.Level] + item.FloorBonus
			item.UpgradeCost, item.NextUnitPrice = &cost, &next
		}
		view.Shops = append(view.Shops, item)
	}
	return view
}

// nextSlot returns the first slot in canonical order that the player can still buy.
func (s *Service) nextSlot(state State) (SlotConfig, bool) {
	for _, id := range s.canonicalSlots(nil) {
		if !s.unlocked(state, id) {
			return s.cfg.Slots[s.slotIndex[id]], true
		}
	}
	return SlotConfig{}, false
}

func fingerprint(cfg Config) string {
	// Config contains only JSON-serializable scalar values and slices.
	encoded, _ := json.Marshal(cfg)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func clone(state State) State {
	shops := state.Shops
	state.UnlockedSlots = append([]string(nil), state.UnlockedSlots...)
	state.Shops = make([]ShopState, len(shops))
	for i, shop := range shops {
		shop.Segments = append([]Segment(nil), shop.Segments...)
		state.Shops[i] = shop
	}
	return state
}
