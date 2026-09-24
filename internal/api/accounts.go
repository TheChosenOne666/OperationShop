package api

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"streetcorner/internal/game"
	"streetcorner/internal/session"
)

// Accounts hands out one game.Service per account key, creating it on first
// sight. The map doubles as the per-account registry: each Service owns exactly
// one save file and serializes its own writes with its mutex (the M02
// concurrency guarantee), so concurrent commands for one account still commit
// once while different accounts never block each other.
//
// A save that fails to load (corrupt or rule change) rejects that account's
// requests only; other accounts keep working, unlike the single-account
// process that refuses to start at all.
type Accounts struct {
	mu       sync.RWMutex
	services map[string]*game.Service
	cfg      game.Config
	dir      string
	now      func() time.Time
}

// NewAccounts prepares the registry. dir holds one save file per account,
// named "<account key>.json"; it is created on the first save.
func NewAccounts(cfg game.Config, dir string, now func() time.Time) *Accounts {
	return &Accounts{services: make(map[string]*game.Service), cfg: cfg, dir: dir, now: now}
}

// Service returns the account's service, creating it on first use. The account
// key is re-validated here because it becomes a file name: both exchangers
// guarantee a safe key, and this check keeps a future exchanger from opening a
// path-traversal hole by accident.
func (a *Accounts) Service(accountKey string) (*game.Service, error) {
	if !session.ValidateAccountKey(accountKey) {
		return nil, fmt.Errorf("account key is not safe as a save file name")
	}
	a.mu.RLock()
	service, ok := a.services[accountKey]
	a.mu.RUnlock()
	if ok {
		return service, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Another goroutine may have created the service while this one waited.
	if service, ok = a.services[accountKey]; ok {
		return service, nil
	}
	service, err := game.NewService(a.cfg, game.FileStore{Path: filepath.Join(a.dir, accountKey+".json")}, a.now)
	if err != nil {
		return nil, err
	}
	a.services[accountKey] = service
	return service, nil
}
