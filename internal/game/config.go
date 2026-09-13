// Package game implements the local prototype's server-authoritative business rules.
package game

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
	_ "time/tzdata" // Keep the configured business calendar available on Windows.
)

// ShopConfig describes a fixed shop position and its provisional revenue rule.
type ShopConfig struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Floor           int    `json:"floor"`
	Slot            int    `json:"slot"`
	CoinsPerVisitor int64  `json:"coinsPerVisitor"`
}

// Config contains versioned prototype rules, not final game-balance decisions.
type Config struct {
	RulesVersion           string       `json:"rulesVersion"`
	InitialCoins           int64        `json:"initialCoins"`
	DailyVisitors          int64        `json:"dailyVisitors"`
	VisitorIntervalSeconds int64        `json:"visitorIntervalSeconds"`
	BusinessTimezone       string       `json:"businessTimezone"`
	Shops                  []ShopConfig `json:"shops"`
}

// LoadConfig reads one strict JSON configuration and validates the module boundary.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open rules: %w", err)
	}
	defer f.Close()
	var cfg Config
	if err := decodeJSON(f, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode rules: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects unsafe values and positions outside the confirmed two-floor layout.
func (c Config) Validate() error {
	if c.RulesVersion == "" || c.InitialCoins < 0 || c.InitialCoins > 1_000_000_000 {
		return fmt.Errorf("invalid rules version or initial coins")
	}
	if c.DailyVisitors < 1 || c.DailyVisitors > 100_000 || c.VisitorIntervalSeconds < 1 || c.VisitorIntervalSeconds > 3600 {
		return fmt.Errorf("visitor limit or interval out of range")
	}
	if c.BusinessTimezone != "Asia/Shanghai" {
		return fmt.Errorf("this module requires Asia/Shanghai business days")
	}
	if _, err := time.LoadLocation(c.BusinessTimezone); err != nil {
		return fmt.Errorf("load business timezone: %w", err)
	}
	expected := map[string][2]int{
		"clothing": {2, 1}, "dessert": {2, 2}, "bookstore": {2, 3},
		"coffee": {1, 1}, "flowers": {1, 2},
	}
	if len(c.Shops) != len(expected) {
		return fmt.Errorf("this module requires five shops")
	}
	for _, shop := range c.Shops {
		position, ok := expected[shop.ID]
		if !ok || position != [2]int{shop.Floor, shop.Slot} || shop.Name == "" || shop.CoinsPerVisitor < 1 || shop.CoinsPerVisitor > 1_000_000 {
			return fmt.Errorf("invalid or duplicate shop: %s", shop.ID)
		}
		delete(expected, shop.ID)
	}
	return nil
}

func decodeJSON(r io.Reader, target any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}
