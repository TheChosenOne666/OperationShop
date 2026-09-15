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

// maxConfigCoins bounds every configured coin amount (initial coins, prices, costs).
const maxConfigCoins int64 = 1_000_000_000

// SlotConfig describes one shop position and the rule that unlocks it.
type SlotConfig struct {
	ID          string `json:"id"`
	Floor       int    `json:"floor"`
	Position    int    `json:"position"`
	ShopID      string `json:"shopId"`
	UnlockOrder int    `json:"unlockOrder"`
	UnlockCost  int64  `json:"unlockCost"`
}

// ShopConfig describes one shop position and its per-level revenue rule.
type ShopConfig struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	Floor                int     `json:"floor"`
	Slot                 int     `json:"slot"`
	LevelCoinsPerVisitor []int64 `json:"levelCoinsPerVisitor"`
}

// Config contains versioned prototype rules, not final game-balance decisions.
// Runtime state (unlocked slots, levels, spending, visitor cap) never lives here:
// the rule fingerprint covers this whole structure, so state fields would make
// every existing save look like a rule change.
type Config struct {
	RulesVersion           string       `json:"rulesVersion"`
	InitialCoins           int64        `json:"initialCoins"`
	BaseVisitors           int64        `json:"baseVisitors"`
	VisitorsPerShop        int64        `json:"visitorsPerShop"`
	FullFloorBonus         int64        `json:"fullFloorBonus"`
	VisitorIntervalSeconds int64        `json:"visitorIntervalSeconds"`
	BusinessTimezone       string       `json:"businessTimezone"`
	Slots                  []SlotConfig `json:"slots"`
	Shops                  []ShopConfig `json:"shops"`
	UpgradeCosts           []int64      `json:"upgradeCosts"`
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

// Validate rejects unsafe values, positions outside the confirmed two-floor layout
// and any slot/shop mapping that is not one-to-one.
func (c Config) Validate() error {
	if c.RulesVersion == "" || c.InitialCoins < 0 || c.InitialCoins > maxConfigCoins {
		return fmt.Errorf("invalid rules version or initial coins")
	}
	if c.BaseVisitors < 1 || c.BaseVisitors > 100_000 || c.VisitorsPerShop < 1 || c.VisitorsPerShop > 100_000 ||
		c.FullFloorBonus < 0 || c.FullFloorBonus > maxConfigCoins ||
		c.VisitorIntervalSeconds < 1 || c.VisitorIntervalSeconds > 3600 {
		return fmt.Errorf("visitor limit, floor bonus or interval out of range")
	}
	if c.BusinessTimezone != "Asia/Shanghai" {
		return fmt.Errorf("this module requires Asia/Shanghai business days")
	}
	if _, err := time.LoadLocation(c.BusinessTimezone); err != nil {
		return fmt.Errorf("load business timezone: %w", err)
	}
	if len(c.Shops) != 5 || len(c.Slots) != 5 {
		return fmt.Errorf("this module requires two floors with five shops and five slots")
	}
	if err := c.validateShops(); err != nil {
		return err
	}
	return c.validateSlots()
}

func (c Config) validateShops() error {
	levels := 0
	positions := make(map[[2]int]string, len(c.Shops))
	for _, shop := range c.Shops {
		if shop.ID == "" || shop.Name == "" || shop.Floor < 1 || shop.Floor > 2 || shop.Slot < 1 {
			return fmt.Errorf("invalid shop: %s", shop.ID)
		}
		position := [2]int{shop.Floor, shop.Slot}
		if positions[position] != "" {
			return fmt.Errorf("duplicate shop position: %s", shop.ID)
		}
		positions[position] = shop.ID
		if len(shop.LevelCoinsPerVisitor) < 1 || len(shop.LevelCoinsPerVisitor) > 100 {
			return fmt.Errorf("shop %s must define one to one hundred levels", shop.ID)
		}
		if levels == 0 {
			levels = len(shop.LevelCoinsPerVisitor)
		} else if len(shop.LevelCoinsPerVisitor) != levels {
			return fmt.Errorf("every shop must define the same number of levels")
		}
		for _, price := range shop.LevelCoinsPerVisitor {
			if price < 1 || price > maxConfigCoins {
				return fmt.Errorf("shop %s has a price out of range", shop.ID)
			}
		}
	}
	if len(c.UpgradeCosts) != levels-1 {
		return fmt.Errorf("upgrade costs must match the configured level count")
	}
	for _, cost := range c.UpgradeCosts {
		if cost < 0 || cost > maxConfigCoins {
			return fmt.Errorf("upgrade cost out of range")
		}
	}
	return nil
}

func (c Config) validateSlots() error {
	positions := make(map[[2]int]string, len(c.Shops))
	for _, shop := range c.Shops {
		positions[[2]int{shop.Floor, shop.Slot}] = shop.ID
	}
	ids := make(map[string]bool, len(c.Slots))
	floors := make(map[int]int, 2)
	orders := make(map[int]bool, len(c.Slots))
	for _, slot := range c.Slots {
		if slot.ID == "" || slot.Floor < 1 || slot.Floor > 2 || slot.Position < 1 ||
			slot.UnlockOrder < 0 || slot.UnlockCost < 0 || slot.UnlockCost > maxConfigCoins {
			return fmt.Errorf("invalid slot: %s", slot.ID)
		}
		if ids[slot.ID] {
			return fmt.Errorf("duplicate slot: %s", slot.ID)
		}
		ids[slot.ID] = true
		floors[slot.Floor]++
		orders[slot.UnlockOrder] = true
		if shopID, ok := positions[[2]int{slot.Floor, slot.Position}]; !ok || shopID != slot.ShopID {
			return fmt.Errorf("slot %s must map to the shop at floor %d position %d", slot.ID, slot.Floor, slot.Position)
		}
		// Ground-floor shops open unlocked; every upper-floor slot is bought in order.
		if (slot.UnlockOrder == 0) != (slot.Floor == 1) {
			return fmt.Errorf("slot %s has an unlock order that contradicts its floor", slot.ID)
		}
	}
	if floors[1] != 2 || floors[2] != 3 {
		return fmt.Errorf("this module requires two ground-floor and three upper-floor slots")
	}
	highest := 0
	for order := range orders {
		if order > highest {
			highest = order
		}
	}
	if len(orders) != highest+1 {
		return fmt.Errorf("unlock order must be contiguous starting at zero")
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
