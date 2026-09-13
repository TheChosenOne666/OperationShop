package game

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// Domain errors are mapped to stable HTTP error codes by the API layer.
var (
	ErrUnknownShop    = errors.New("unknown shop")
	ErrClockBackwards = errors.New("server clock moved backwards")
	ErrNumericLimit   = errors.New("safe integer limit reached")
)

const maxSafeInteger int64 = 1<<53 - 1

// ShopState records cumulative server-calculated income and preparation status.
type ShopState struct {
	ID       string `json:"id"`
	Prepared bool   `json:"prepared"`
	Visitors int64  `json:"visitors"`
	Revenue  int64  `json:"revenue"`
}

// State is the versioned save for the single local development account.
type State struct {
	SchemaVersion     int         `json:"schemaVersion"`
	RulesFingerprint  string      `json:"rulesFingerprint"`
	Revision          int64       `json:"revision"`
	Coins             int64       `json:"coins"`
	BusinessDay       string      `json:"businessDay"`
	VisitorsRemaining int64       `json:"visitorsRemaining"`
	LastAccrualAt     time.Time   `json:"lastAccrualAt"`
	LastObservedAt    time.Time   `json:"lastObservedAt"`
	NextShopIndex     int         `json:"nextShopIndex"`
	Shops             []ShopState `json:"shops"`
}

// Result describes one atomic operation, including newly served visitors and coins.
type Result struct {
	State        State `json:"state"`
	EarnedCoins  int64 `json:"earnedCoins"`
	VisitorsUsed int64 `json:"visitorsUsed"`
	Changed      bool  `json:"changed"`
}

// Service serializes mutations and commits the save before publishing a new state.
// Exactly one Service process may own a save file.
type Service struct {
	mu    sync.Mutex
	cfg   Config
	zone  *time.Location
	store Store
	now   func() time.Time
	state State
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
	zone, err := time.LoadLocation(cfg.BusinessTimezone)
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, store: store, zone: zone, now: now}
	state, err := store.Load()
	if errors.Is(err, os.ErrNotExist) {
		stamp := now().UTC()
		state = State{
			SchemaVersion: 1, RulesFingerprint: fingerprint(cfg), Revision: 1,
			Coins: cfg.InitialCoins, BusinessDay: stamp.In(zone).Format(time.DateOnly),
			VisitorsRemaining: cfg.DailyVisitors, LastAccrualAt: stamp, LastObservedAt: stamp,
		}
		for _, shop := range cfg.Shops {
			state.Shops = append(state.Shops, ShopState{ID: shop.ID})
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
	s.state = clone(state)
	return s, nil
}

// Configuration returns an isolated copy of the active prototype rules.
func (s *Service) Configuration() Config {
	cfg := s.cfg
	cfg.Shops = append([]ShopConfig(nil), cfg.Shops...)
	return cfg
}

// Snapshot returns the last committed state without settling income or refreshing a day.
func (s *Service) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.state)
}

// Prepare opens one shop after settling the interval before that shop was prepared.
// Repeating preparation for an already open shop has no effect.
func (s *Service) Prepare(id string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, shop := range s.state.Shops {
		if shop.ID == id {
			index = i
			break
		}
	}
	if index == -1 {
		return Result{}, ErrUnknownShop
	}
	if s.state.Shops[index].Prepared {
		return Result{State: clone(s.state)}, nil
	}
	return s.mutate(index)
}

// Settle awards only configured income using the server clock and available visitors.
func (s *Service) Settle() (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(-1)
}

func (s *Service) mutate(prepareIndex int) (Result, error) {
	now := s.now().UTC()
	if now.Before(s.state.LastObservedAt) {
		return Result{}, ErrClockBackwards
	}
	next := clone(s.state)
	result := Result{}
	if err := s.accrue(&next, now, &result); err != nil {
		return Result{}, err
	}
	if prepareIndex >= 0 {
		next.Shops[prepareIndex].Prepared = true
	}
	next.LastObservedAt = now
	if now.Equal(s.state.LastObservedAt) && prepareIndex < 0 {
		return Result{State: clone(s.state)}, nil
	}
	if next.Revision == maxSafeInteger {
		return Result{}, ErrNumericLimit
	}
	next.Revision++
	if err := s.store.Save(next); err != nil {
		return Result{}, fmt.Errorf("commit save: %w", err)
	}
	s.state = next
	result.State, result.Changed = clone(next), true
	return result, nil
}

func (s *Service) accrue(state *State, now time.Time, result *Result) error {
	local := now.In(s.zone)
	day := local.Format(time.DateOnly)
	if state.BusinessDay != day {
		state.BusinessDay = day
		state.VisitorsRemaining = s.cfg.DailyVisitors
		// Past business days are not backfilled; no multi-day offline rewards in M01.
		state.LastAccrualAt = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.zone).UTC()
	}
	active := false
	for _, shop := range state.Shops {
		active = active || shop.Prepared
	}
	if !active || state.VisitorsRemaining == 0 {
		state.LastAccrualAt = now
		return nil
	}
	interval := time.Duration(s.cfg.VisitorIntervalSeconds) * time.Second
	elapsed := now.Sub(state.LastAccrualAt)
	visits := min(int64(elapsed/interval), state.VisitorsRemaining)
	state.LastAccrualAt = now.Add(-(elapsed % interval))
	for range visits {
		for !state.Shops[state.NextShopIndex].Prepared {
			state.NextShopIndex = (state.NextShopIndex + 1) % len(state.Shops)
		}
		index := state.NextShopIndex
		amount := s.cfg.Shops[index].CoinsPerVisitor
		if state.Coins > maxSafeInteger-amount || state.Shops[index].Visitors == maxSafeInteger {
			return ErrNumericLimit
		}
		state.Coins += amount
		state.Shops[index].Revenue += amount
		state.Shops[index].Visitors++
		state.VisitorsRemaining--
		result.EarnedCoins += amount
		result.VisitorsUsed++
		state.NextShopIndex = (index + 1) % len(state.Shops)
	}
	return nil
}

func (s *Service) validateState(state State) error {
	if state.SchemaVersion != 1 || state.RulesFingerprint != fingerprint(s.cfg) {
		return fmt.Errorf("schema or rules changed; use a separate development save")
	}
	if state.Revision < 1 || state.Revision > maxSafeInteger || state.Coins < s.cfg.InitialCoins || state.Coins > maxSafeInteger ||
		state.VisitorsRemaining < 0 || state.VisitorsRemaining > s.cfg.DailyVisitors ||
		state.NextShopIndex < 0 || state.NextShopIndex >= len(s.cfg.Shops) || len(state.Shops) != len(s.cfg.Shops) {
		return fmt.Errorf("state counters out of range")
	}
	if state.LastAccrualAt.IsZero() || state.LastObservedAt.IsZero() || state.LastAccrualAt.After(state.LastObservedAt) ||
		state.BusinessDay != state.LastObservedAt.In(s.zone).Format(time.DateOnly) ||
		state.BusinessDay != state.LastAccrualAt.In(s.zone).Format(time.DateOnly) {
		return fmt.Errorf("invalid settlement timestamps")
	}
	total := s.cfg.InitialCoins
	for i, shop := range state.Shops {
		price := s.cfg.Shops[i].CoinsPerVisitor
		if shop.ID != s.cfg.Shops[i].ID || shop.Visitors < 0 || shop.Visitors > maxSafeInteger/price ||
			shop.Revenue != shop.Visitors*price || (!shop.Prepared && shop.Visitors != 0) || total > math.MaxInt64-shop.Revenue {
			return fmt.Errorf("invalid shop accounting: %s", shop.ID)
		}
		total += shop.Revenue
	}
	if total != state.Coins {
		return fmt.Errorf("coin total does not match shop revenue")
	}
	return nil
}

func fingerprint(cfg Config) string {
	// Config contains only JSON-serializable scalar values and slices.
	encoded, _ := json.Marshal(cfg)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func clone(state State) State {
	state.Shops = append([]ShopState(nil), state.Shops...)
	return state
}
