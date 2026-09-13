package game

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码测试数据失败：%v", err)
	}
	return data
}

// TestDevelopmentConfig 验证当前开发配置的两层五店、初始资源与收益表。
func TestDevelopmentConfig(t *testing.T) {
	cfg := testConfig(t)
	want := Config{
		RulesVersion: "m01-prototype-v1", InitialCoins: 1280, DailyVisitors: 50,
		VisitorIntervalSeconds: 5, BusinessTimezone: "Asia/Shanghai",
		Shops: []ShopConfig{
			{ID: "clothing", Name: "衣橱", Floor: 2, Slot: 1, CoinsPerVisitor: 12},
			{ID: "dessert", Name: "甜屋", Floor: 2, Slot: 2, CoinsPerVisitor: 8},
			{ID: "bookstore", Name: "书店", Floor: 2, Slot: 3, CoinsPerVisitor: 10},
			{ID: "coffee", Name: "咖啡", Floor: 1, Slot: 1, CoinsPerVisitor: 6},
			{ID: "flowers", Name: "花束", Floor: 1, Slot: 2, CoinsPerVisitor: 9},
		},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("开发规则不符\n实际：%+v\n期望：%+v", cfg, want)
	}
	floors := map[int]int{}
	var roundCoins int64
	for _, shop := range cfg.Shops {
		floors[shop.Floor]++
		roundCoins += shop.CoinsPerVisitor
	}
	if !reflect.DeepEqual(floors, map[int]int{1: 2, 2: 3}) || roundCoins != 45 {
		t.Fatalf("楼层分布或每轮收益错误：floors=%v，coins=%d", floors, roundCoins)
	}
}

// TestConfigValidate 验证配置边界、固定店铺位置及重复店铺拒绝规则。
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "默认配置", mutate: func(*Config) {}},
		{name: "合法下界", mutate: func(c *Config) {
			c.InitialCoins, c.DailyVisitors, c.VisitorIntervalSeconds = 0, 1, 1
			for i := range c.Shops {
				c.Shops[i].CoinsPerVisitor = 1
			}
		}},
		{name: "合法上界", mutate: func(c *Config) {
			c.InitialCoins, c.DailyVisitors, c.VisitorIntervalSeconds = 1_000_000_000, 100_000, 3600
			for i := range c.Shops {
				c.Shops[i].CoinsPerVisitor = 1_000_000
			}
		}},
		{name: "合法调整店铺顺序", mutate: func(c *Config) { c.Shops[0], c.Shops[4] = c.Shops[4], c.Shops[0] }},
		{"缺失规则版本", func(c *Config) { c.RulesVersion = "" }, true},
		{"负初始金币", func(c *Config) { c.InitialCoins = -1 }, true},
		{"初始金币越界", func(c *Config) { c.InitialCoins = 1_000_000_001 }, true},
		{"零日客流", func(c *Config) { c.DailyVisitors = 0 }, true},
		{"负日客流", func(c *Config) { c.DailyVisitors = -1 }, true},
		{"日客流越界", func(c *Config) { c.DailyVisitors = 100_001 }, true},
		{"零间隔", func(c *Config) { c.VisitorIntervalSeconds = 0 }, true},
		{"负间隔", func(c *Config) { c.VisitorIntervalSeconds = -1 }, true},
		{"间隔越界", func(c *Config) { c.VisitorIntervalSeconds = 3601 }, true},
		{"缺失时区", func(c *Config) { c.BusinessTimezone = "" }, true},
		{"禁止UTC时区", func(c *Config) { c.BusinessTimezone = "UTC" }, true},
		{"无效时区", func(c *Config) { c.BusinessTimezone = "Invalid/Timezone" }, true},
		{"空店铺列表", func(c *Config) { c.Shops = nil }, true},
		{"少于五店", func(c *Config) { c.Shops = c.Shops[:4] }, true},
		{"多于五店", func(c *Config) { c.Shops = append(c.Shops, c.Shops[0]) }, true},
		{"重复店铺", func(c *Config) { c.Shops[1] = c.Shops[0] }, true},
		{"未知店铺", func(c *Config) { c.Shops[0].ID = "unknown" }, true},
		{"缺失店铺ID", func(c *Config) { c.Shops[0].ID = "" }, true},
		{"缺失店名", func(c *Config) { c.Shops[0].Name = "" }, true},
		{"错误楼层", func(c *Config) { c.Shops[0].Floor = 1 }, true},
		{"超出两层", func(c *Config) { c.Shops[0].Floor = 3 }, true},
		{"错误店位", func(c *Config) { c.Shops[0].Slot = 2 }, true},
		{"零店位", func(c *Config) { c.Shops[0].Slot = 0 }, true},
		{"零收益", func(c *Config) { c.Shops[0].CoinsPerVisitor = 0 }, true},
		{"负收益", func(c *Config) { c.Shops[0].CoinsPerVisitor = -1 }, true},
		{"收益越界", func(c *Config) { c.Shops[0].CoinsPerVisitor = 1_000_001 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			tc.mutate(&cfg)
			before := string(testJSON(t, cfg))
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v，期望错误=%t", err, tc.wantErr)
			}
			if string(testJSON(t, cfg)) != before {
				t.Fatal("校验不能修改输入配置")
			}
		})
	}
}

// TestLoadConfigStrictJSON 验证配置文件严格 JSON 解码及语义校验。
func TestLoadConfigStrictJSON(t *testing.T) {
	cfg := testConfig(t)
	valid := string(testJSON(t, cfg))
	invalidConfig := testConfig(t)
	invalidConfig.DailyVisitors = 0
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"合法配置", valid, false},
		{"允许末尾空白", valid + " \n\t", false},
		{"空文件", "", true},
		{"截断JSON", valid[:len(valid)-1], true},
		{"非法JSON", "{broken", true},
		{"顶层未知字段", strings.TrimSuffix(valid, "}") + `,"unknown":true}`, true},
		{"店铺未知字段", strings.Replace(valid, `"id":"clothing"`, `"id":"clothing","unknown":true`, 1), true},
		{"多个JSON值", valid + "\n{}", true},
		{"尾随null", valid + " null", true},
		{"尾随垃圾", valid + " garbage", true},
		{"null不能作为配置", "null", true},
		{"顶层数组", "[]", true},
		{"错误字段类型", strings.Replace(valid, `"initialCoins":1280`, `"initialCoins":"1280"`, 1), true},
		{"缺失必填字段", "{}", true},
		{"不合法规则", string(testJSON(t, invalidConfig)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rules.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadConfig(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadConfig() = %v，期望错误=%t", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, cfg) {
				t.Fatalf("配置往返不一致：%+v", got)
			}
			if tc.wantErr && !reflect.DeepEqual(got, Config{}) {
				t.Fatalf("加载失败不应返回部分配置：%+v", got)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.body {
				t.Fatalf("读取配置不能修改原文件：%v", err)
			}
		})
	}
	t.Run("配置文件不存在", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.json")
		_, err := LoadConfig(path)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("应保留文件不存在错误：%v", err)
		}
	})
}

// TestChangedRulesDoNotReuseSave 验证有效配置变化后不能复用旧规则存档。
func TestChangedRulesDoNotReuseSave(t *testing.T) {
	service, _, _ := newTestService(t)
	original := service.Snapshot()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"规则版本", func(c *Config) { c.RulesVersion += "-next" }},
		{"初始金币", func(c *Config) { c.InitialCoins-- }},
		{"日客流", func(c *Config) { c.DailyVisitors++ }},
		{"结算间隔", func(c *Config) { c.VisitorIntervalSeconds++ }},
		{"收益规则", func(c *Config) { c.Shops[0].CoinsPerVisitor++ }},
		{"店铺名称", func(c *Config) { c.Shops[0].Name += "新" }},
		{"店铺顺序", func(c *Config) { c.Shops[0], c.Shops[4] = c.Shops[4], c.Shops[0] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			tc.mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("指纹测试必须使用合法新配置：%v", err)
			}
			store := &memoryStore{state: copyTestState(original), exists: true}
			loaded, err := NewService(cfg, store, testTime)
			if loaded != nil || err == nil {
				t.Fatal("规则指纹变化必须拒绝旧存档")
			}
			if store.saveCount() != 0 {
				t.Fatal("规则不匹配不能覆盖旧存档")
			}
			assertTestStoredState(t, store, original)
		})
	}
}
