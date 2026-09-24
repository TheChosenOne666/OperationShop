// Package session issues and validates the server's own session tokens after a
// WeChat login code has been exchanged for an account key. Tokens live in
// process memory only: a restart invalidates every token, which clients absorb
// by logging in again (the save itself is on disk, so no data is lost).
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Errors distinguish a token that never existed from one that aged out, so the
// API layer can report SESSION_INVALID and SESSION_EXPIRED separately.
var (
	ErrInvalidToken = errors.New("session token not found")
	ErrExpiredToken = errors.New("session token expired")
)

// tokenBytes is the entropy of one issued token: 256 bits, hex encoded.
const tokenBytes = 32

// Store keeps issued tokens in memory. It is safe for concurrent use.
type Store struct {
	mu     sync.RWMutex
	tokens map[string]entry
	ttl    time.Duration
	now    func() time.Time
}

// entry is one live token: which account it belongs to and when it ages out.
type entry struct {
	accountKey string
	expiresAt  time.Time
}

// NewStore creates an empty token store with a fixed lifetime and clock.
func NewStore(ttl time.Duration, now func() time.Time) (*Store, error) {
	if ttl <= 0 || now == nil {
		return nil, fmt.Errorf("a positive session lifetime and a clock are required")
	}
	return &Store{tokens: make(map[string]entry), ttl: ttl, now: now}, nil
}

// Issue mints a fresh token for one account and sweeps expired tokens first,
// which bounds memory to live tokens plus those issued since the last sweep.
// The same account can hold several concurrent tokens (multiple devices).
func (s *Store) Issue(accountKey string) (string, error) {
	if accountKey == "" {
		return "", fmt.Errorf("an account key is required to issue a token")
	}
	secret := make([]byte, tokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate token entropy: %w", err)
	}
	token := hex.EncodeToString(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.tokens[token] = entry{accountKey: accountKey, expiresAt: s.now().Add(s.ttl)}
	return token, nil
}

// Authenticate resolves a token to its account key. A token that aged out but
// was not swept yet reports ErrExpiredToken; a swept or unknown token reports
// ErrInvalidToken. Both are 401 for the client; the split exists for diagnostics.
func (s *Store) Authenticate(token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	s.mu.RLock()
	found, ok := s.tokens[token]
	s.mu.RUnlock()
	if !ok {
		return "", ErrInvalidToken
	}
	if !s.now().Before(found.expiresAt) {
		return "", ErrExpiredToken
	}
	return found.accountKey, nil
}

// sweepLocked removes expired tokens. The caller must hold the write lock.
func (s *Store) sweepLocked() {
	now := s.now()
	for token, found := range s.tokens {
		if !now.Before(found.expiresAt) {
			delete(s.tokens, token)
		}
	}
}

// liveTokens reports how many tokens the store currently holds; tests use it to
// prove the sweep actually reclaims entries instead of trusting the map.
func (s *Store) liveTokens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens)
}

// AccountKey derives the storage key for one WeChat openid. The openid itself
// never reaches a filename, a log line or the save: only this truncated hash
// does, so a leaked save directory cannot be mapped back to WeChat accounts.
func AccountKey(openid string) string {
	sum := sha256.Sum256([]byte(openid))
	return hex.EncodeToString(sum[:16])
}
