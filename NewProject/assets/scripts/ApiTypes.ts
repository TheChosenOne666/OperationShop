// 服务端接口的客户端镜像。**逐字段对齐 Go 侧的 json tag**，不做任何本地计算。
// 依据 ADR 0001「服务端权威与客户端零计算」：这里只描述"服务端告诉我们什么"，
// 不出现单价表、客流公式、解锁价之类任何规则常量——那些归 config/development.json。
//
// 对齐来源（改动服务端结构时必须同步本文件）：
//   internal/game/service.go   —— MallView / ShopView / Result
//   internal/game/config.go    —— Config / SlotConfig / ShopConfig

/** 一个铺位的静态配置。 */
export interface SlotConfig {
    id: string;
    floor: number;
    position: number;
    shopId: string;
    unlockOrder: number;
    unlockCost: number;
}

/** 一家店的静态配置。levelCoinsPerVisitor 是服务端用的等级单价，客户端只读不推。 */
export interface ShopConfig {
    id: string;
    name: string;
    floor: number;
    slot: number;
    levelCoinsPerVisitor: number[];
}

/** GET /api/v1/config 的响应体。 */
export interface Config {
    rulesVersion: string;
    initialCoins: number;
    baseVisitors: number;
    visitorsPerShop: number;
    fullFloorBonus: number;
    visitorIntervalSeconds: number;
    businessTimezone: string;
    slots: SlotConfig[];
    shops: ShopConfig[];
    upgradeCosts: number[];
}

/** 一家店的运行时状态。 */
export interface ShopView {
    id: string;
    name: string;
    floor: number;
    slot: number;
    prepared: boolean;
    level: number;
    /**
     * 该店**当前**的单客收益档位，已含满铺加成。
     * ⚠️ 它**不是**「本轮客人按什么价付的」：服务端先结算后应用命令，同一条 Prepare/Upgrade
     * 响应里这里是加价后的新价，客人却按加价前的旧价入账（口径见 ADR 0001 与
     * `internal/game/service.go` 的 ShopView 注释）。要显示成交价就读 `Arrival.unitPrice`。
     */
    unitPrice: number;
    /** 等级单价那一半。`unitPriceBase + floorBonus === unitPrice`，两半都由服务端给。 */
    unitPriceBase: number;
    /** 该店当前**实际生效**的满铺加成，未满铺为 0。客户端不得自己判满铺（GDD §6.2）。 */
    floorBonus: number;
    visitors: number;
    revenue: number;
    /** null 表示已满级，没有下一级可升。 */
    upgradeCost: number | null;
    /** 升级一级后的单客收益；已满级时为 null。满铺状态与等级无关，所以加成沿用同一个。 */
    nextUnitPrice: number | null;
}

/** GET /api/v1/mall 的响应体，也是 Result.state。 */
export interface MallView {
    schemaVersion: number;
    rulesFingerprint: string;
    revision: number;
    coins: number;
    spent: number;
    /**
     * 当前业务日内的到店进账。跨业务日由服务端归零，解锁与升级的支出**不**从里面扣，
     * 所以经营页那一格可以原样显示，不必客户端拿金币做差猜。
     */
    todayEarned: number;
    businessDay: string;
    capFrozen: boolean;
    /**
     * 当日客流上限。**服务端刻意用 null 而不是 0** 表示"当天还没冻结、上限尚不适用"，
     * 客户端不得把它当 0 显示（否则 HUD 会闪出 0/0）。
     */
    dailyVisitorCap: number | null;
    visitorsServed: number;
    visitorsRemaining: number;
    lastAccrualAt: string;
    /** 语义是「最后一次**发布状态**的时刻」，不是「最后一次被轮询的时刻」——不要用它做心跳判断。 */
    lastObservedAt: string;
    unlockedSlots: string[];
    nextSlotId: string | null;
    nextUnlockCost: number | null;
    shops: ShopView[];
}

/** 写命令（prepare / settle / unlock / upgrade）的统一响应体。 */
export interface Result {
    state: MallView;
    earnedCoins: number;
    visitorsUsed: number;
    /** false 表示这次是空转：服务端没改状态、没落盘、revision 也没变。 */
    changed: boolean;
}
