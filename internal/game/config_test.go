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

// TestDevelopmentConfig 验证 v2 开发配置的两层五铺、解锁表与等级单价表。
func TestDevelopmentConfig(t *testing.T) {
	cfg := testConfig(t)
	want := Config{
		RulesVersion: "m02-v2", InitialCoins: 1280, BaseVisitors: 20, VisitorsPerShop: 15,
		FullFloorBonus: 1, VisitorIntervalSeconds: 5, BusinessTimezone: "Asia/Shanghai",
		Slots: []SlotConfig{
			{ID: "f1-s1", Floor: 1, Position: 1, ShopID: "coffee", UnlockOrder: 0, UnlockCost: 0},
			{ID: "f1-s2", Floor: 1, Position: 2, ShopID: "flowers", UnlockOrder: 0, UnlockCost: 0},
			{ID: "f2-s1", Floor: 2, Position: 1, ShopID: "clothing", UnlockOrder: 1, UnlockCost: 600},
			{ID: "f2-s2", Floor: 2, Position: 2, ShopID: "dessert", UnlockOrder: 2, UnlockCost: 800},
			{ID: "f2-s3", Floor: 2, Position: 3, ShopID: "bookstore", UnlockOrder: 3, UnlockCost: 1200},
		},
		Shops: []ShopConfig{
			{ID: "coffee", Name: "咖啡", Floor: 1, Slot: 1, LevelCoinsPerVisitor: []int64{6, 7, 8, 10, 11}},
			{ID: "flowers", Name: "花束", Floor: 1, Slot: 2, LevelCoinsPerVisitor: []int64{9, 10, 12, 14, 16}},
			{ID: "clothing", Name: "衣橱", Floor: 2, Slot: 1, LevelCoinsPerVisitor: []int64{12, 14, 16, 19, 22}},
			{ID: "dessert", Name: "甜屋", Floor: 2, Slot: 2, LevelCoinsPerVisitor: []int64{8, 9, 11, 13, 15}},
			{ID: "bookstore", Name: "书店", Floor: 2, Slot: 3, LevelCoinsPerVisitor: []int64{10, 12, 14, 16, 19}},
		},
		UpgradeCosts: []int64{300, 700, 1200, 2000},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("开发规则不符\n实际：%+v\n期望：%+v", cfg, want)
	}
	floors := map[int]int{}
	for _, shop := range cfg.Shops {
		floors[shop.Floor]++
	}
	if !reflect.DeepEqual(floors, map[int]int{1: 2, 2: 3}) {
		t.Fatalf("楼层分布错误：%v", floors)
	}
	// 轮询顺序由数组顺序决定，必须一层临街优先（GDD §2.3 / 拍板决策 7）。
	for i, id := range []string{"coffee", "flowers", "clothing", "dessert", "bookstore"} {
		if cfg.Shops[i].ID != id {
			t.Fatalf("轮询顺序第 %d 位 = %q，期望 %q", i, cfg.Shops[i].ID, id)
		}
	}
}

// TestConfigValidate 验证配置边界、固定布局、解锁顺序与铺位/店铺一对一映射。
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "默认配置", mutate: func(*Config) {}},
		{name: "合法下界", mutate: func(c *Config) {
			c.InitialCoins, c.BaseVisitors, c.VisitorsPerShop, c.FullFloorBonus, c.VisitorIntervalSeconds = 0, 1, 1, 0, 1
			for i := range c.Shops {
				c.Shops[i].LevelCoinsPerVisitor = []int64{1, 1, 1, 1, 1}
			}
			for i := range c.UpgradeCosts {
				c.UpgradeCosts[i] = 0
			}
			for i := range c.Slots {
				c.Slots[i].UnlockCost = 0
			}
		}},
		{name: "合法上界", mutate: func(c *Config) {
			c.InitialCoins, c.BaseVisitors, c.VisitorsPerShop, c.VisitorIntervalSeconds = 1_000_000_000, 100_000, 100_000, 3600
			c.FullFloorBonus = 1_000_000_000
			for i := range c.Shops {
				c.Shops[i].LevelCoinsPerVisitor = []int64{1_000_000_000, 1_000_000_000, 1_000_000_000, 1_000_000_000, 1_000_000_000}
			}
			for i := range c.UpgradeCosts {
				c.UpgradeCosts[i] = 1_000_000_000
			}
			for i := range c.Slots {
				c.Slots[i].UnlockCost = 1_000_000_000
			}
		}},
		{name: "合法调整轮询顺序", mutate: func(c *Config) { c.Shops[0], c.Shops[4] = c.Shops[4], c.Shops[0] }},
		{name: "合法调整解锁价格", mutate: func(c *Config) { c.Slots[2].UnlockCost = 1 }},
		{name: "合法调整等级数", mutate: func(c *Config) {
			for i := range c.Shops {
				c.Shops[i].LevelCoinsPerVisitor = []int64{1, 2}
			}
			c.UpgradeCosts = []int64{5}
		}},
		{"缺失规则版本", func(c *Config) { c.RulesVersion = "" }, true},
		{"负初始金币", func(c *Config) { c.InitialCoins = -1 }, true},
		{"初始金币越界", func(c *Config) { c.InitialCoins = 1_000_000_001 }, true},
		{"零基础客流", func(c *Config) { c.BaseVisitors = 0 }, true},
		{"负基础客流", func(c *Config) { c.BaseVisitors = -1 }, true},
		{"基础客流越界", func(c *Config) { c.BaseVisitors = 100_001 }, true},
		{"零单店客流", func(c *Config) { c.VisitorsPerShop = 0 }, true},
		{"负单店客流", func(c *Config) { c.VisitorsPerShop = -1 }, true},
		{"单店客流越界", func(c *Config) { c.VisitorsPerShop = 100_001 }, true},
		{"负满铺加成", func(c *Config) { c.FullFloorBonus = -1 }, true},
		{"零间隔", func(c *Config) { c.VisitorIntervalSeconds = 0 }, true},
		{"负间隔", func(c *Config) { c.VisitorIntervalSeconds = -1 }, true},
		{"间隔越界", func(c *Config) { c.VisitorIntervalSeconds = 3601 }, true},
		{"缺失时区", func(c *Config) { c.BusinessTimezone = "" }, true},
		{"禁止UTC时区", func(c *Config) { c.BusinessTimezone = "UTC" }, true},
		{"无效时区", func(c *Config) { c.BusinessTimezone = "Invalid/Timezone" }, true},
		{"空店铺列表", func(c *Config) { c.Shops = nil }, true},
		{"少于五店", func(c *Config) { c.Shops = c.Shops[:4] }, true},
		{"多于五店", func(c *Config) { c.Shops = append(c.Shops, c.Shops[0]) }, true},
		{"空铺位列表", func(c *Config) { c.Slots = nil }, true},
		{"少于五铺位", func(c *Config) { c.Slots = c.Slots[:4] }, true},
		{"重复店铺", func(c *Config) { c.Shops[1] = c.Shops[0] }, true},
		{"未知店铺", func(c *Config) { c.Shops[0].ID = "unknown" }, true},
		{"缺失店铺ID", func(c *Config) { c.Shops[0].ID = "" }, true},
		{"缺失店名", func(c *Config) { c.Shops[0].Name = "" }, true},
		{"错误楼层", func(c *Config) { c.Shops[0].Floor = 3 }, true},
		{"零店位", func(c *Config) { c.Shops[0].Slot = 0 }, true},
		{"店铺位置重复", func(c *Config) { c.Shops[1].Slot = c.Shops[0].Slot }, true},
		{"空等级表", func(c *Config) { c.Shops[0].LevelCoinsPerVisitor = nil }, true},
		{"等级数不一致", func(c *Config) { c.Shops[0].LevelCoinsPerVisitor = []int64{1, 2} }, true},
		{"等级数过多", func(c *Config) {
			levels := make([]int64, 101)
			for i := range levels {
				levels[i] = 1
			}
			c.Shops[0].LevelCoinsPerVisitor = levels
		}, true},
		{"零单价", func(c *Config) { c.Shops[0].LevelCoinsPerVisitor[0] = 0 }, true},
		{"负单价", func(c *Config) { c.Shops[0].LevelCoinsPerVisitor[2] = -1 }, true},
		{"单价越界", func(c *Config) { c.Shops[0].LevelCoinsPerVisitor[4] = 1_000_000_001 }, true},
		{"升级成本数量不符", func(c *Config) { c.UpgradeCosts = c.UpgradeCosts[:3] }, true},
		{"负升级成本", func(c *Config) { c.UpgradeCosts[0] = -1 }, true},
		{"升级成本越界", func(c *Config) { c.UpgradeCosts[3] = 1_000_000_001 }, true},
		{"重复铺位ID", func(c *Config) { c.Slots[1].ID = c.Slots[0].ID }, true},
		{"缺失铺位ID", func(c *Config) { c.Slots[0].ID = "" }, true},
		{"铺位楼层越界", func(c *Config) { c.Slots[0].Floor = 3 }, true},
		{"铺位位置为零", func(c *Config) { c.Slots[0].Position = 0 }, true},
		{"铺位位置重复", func(c *Config) { c.Slots[3].Position = c.Slots[2].Position }, true},
		{"负解锁顺序", func(c *Config) { c.Slots[2].UnlockOrder = -1 }, true},
		{"负解锁价格", func(c *Config) { c.Slots[2].UnlockCost = -1 }, true},
		{"解锁价格越界", func(c *Config) { c.Slots[2].UnlockCost = 1_000_000_001 }, true},
		{"解锁顺序不连续", func(c *Config) { c.Slots[4].UnlockOrder = 7 }, true},
		{"一层铺位未开局解锁", func(c *Config) { c.Slots[0].UnlockOrder = 1 }, true},
		{"二层铺位开局解锁", func(c *Config) { c.Slots[2].UnlockOrder = 0 }, true},
		{"铺位指向未知店铺", func(c *Config) { c.Slots[2].ShopID = "unknown" }, true},
		{"铺位与店铺位置不符", func(c *Config) { c.Slots[2].Position = 3 }, true},
		{"一层三铺位", func(c *Config) { c.Slots[2].Floor, c.Slots[2].Position, c.Slots[2].ShopID = 1, 3, "coffee" }, true},
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
	invalidConfig.BaseVisitors = 0
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
		{"店铺未知字段", strings.Replace(valid, `"id":"coffee"`, `"id":"coffee","unknown":true`, 1), true},
		{"铺位未知字段", strings.Replace(valid, `"shopId":"coffee"`, `"shopId":"coffee","unknown":true`, 1), true},
		{"多个JSON值", valid + "\n{}", true},
		{"尾随null", valid + " null", true},
		{"尾随垃圾", valid + " garbage", true},
		{"null不能作为配置", "null", true},
		{"顶层数组", "[]", true},
		{"错误字段类型", strings.Replace(valid, `"initialCoins":1280`, `"initialCoins":"1280"`, 1), true},
		{"等级表类型错误", strings.Replace(valid, `[6,7,8,10,11]`, `6`, 1), true},
		{"缺失必填字段", "{}", true},
		{"旧版日客流字段被拒绝", strings.Replace(valid, `"baseVisitors":20`, `"dailyVisitors":50`, 1), true},
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
	original := service.snapshotState()
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"规则版本", func(c *Config) { c.RulesVersion += "-next" }},
		{"初始金币", func(c *Config) { c.InitialCoins-- }},
		{"基础客流", func(c *Config) { c.BaseVisitors++ }},
		{"单店客流", func(c *Config) { c.VisitorsPerShop++ }},
		{"满铺加成", func(c *Config) { c.FullFloorBonus++ }},
		{"结算间隔", func(c *Config) { c.VisitorIntervalSeconds++ }},
		{"等级单价", func(c *Config) { c.Shops[0].LevelCoinsPerVisitor[0]++ }},
		{"升级成本", func(c *Config) { c.UpgradeCosts[0]++ }},
		{"解锁价格", func(c *Config) { c.Slots[2].UnlockCost++ }},
		{"解锁顺序", func(c *Config) {
			c.Slots[2].UnlockOrder, c.Slots[3].UnlockOrder = c.Slots[3].UnlockOrder, c.Slots[2].UnlockOrder
		}},
		{"店铺名称", func(c *Config) { c.Shops[0].Name += "新" }},
		{"轮询顺序", func(c *Config) { c.Shops[0], c.Shops[4] = c.Shops[4], c.Shops[0] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			tc.mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("指纹测试必须使用合法新配置：%v", err)
			}
			store := &memoryStore{state: clone(original), exists: true}
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
