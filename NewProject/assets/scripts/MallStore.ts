// M05 编排层：把「什么时候问服务端、拿到响应信不信、这轮谁来了」三件事收在一处。
//
// **本文件刻意不 import "cc"、也不 import ApiClient**：
//   · 不依赖引擎，才能用 Node 自带的类型擦除直接跑 tools/m05_probe.ts 的断言，
//     不必为此引入 jest 之类的新依赖；
//   · 不依赖 ApiClient，就碰不到构建期注入的 BuildConfig（含开发令牌，不入库）。
// 传输与界面都靠注入：MallScene 把真实 ApiClient 与自己（实现 StoreObserver）传进来。
// ⚠️ 因此这里不能用 TS 的构造器参数属性（`constructor(private x)`）与 enum——
//    它们需要代码生成，Node 的类型擦除会直接报错。字段一律显式声明后赋值。
//
// 依据：
//   ADR 0001 服务端权威 / 客户端零计算 —— 只搬运与比较服务端给的字段，
//       不出现单价、客流上限、解锁价等任何规则常量（唯一的运算是累计计数做差，见 diffArrivals）。
//   《玩法与技术方案》B3.6 —— 主界面每 5 秒 settle、切后台停轮询、回前台立即取权威状态。
//   《开发规划文档》§M05 验收第 3 条 —— 界面数值永远以最近一次服务端响应为准，不跳变不回退。
import type { MallView, Result } from "./ApiTypes";

/** 一次请求的来源，只用于日志与表现分层。 */
export type StoreOp = "snapshot" | "settle" | "prepare";

/** 本轮结算里客人的归属：某店来了几位。由两份快照的 shops[].visitors 相减得出。 */
export interface Arrival {
    shopId: string;
    visitors: number;
}

/** 交给界面层的一次更新。数值一律以 view 为准，arrivals 只驱动表现。 */
export interface StoreUpdate {
    view: MallView;
    arrivals: Arrival[];
    /** 本次响应报告的收益总额；快照路径没有这个字段，恒为 0。 */
    earnedCoins: number;
    op: StoreOp;
}

/** 传输层抽象。真实实现是 ApiClient 的三个函数，探针里换成可控的假实现。 */
export interface StoreTransport {
    fetchMall(): Promise<MallView>;
    settle(): Promise<Result>;
    prepareShop(shopId: string): Promise<Result>;
}

/** 界面层回调。错误只给稳定错误码，不给中文提示（与 ApiClient 同口径）。 */
export interface StoreObserver {
    onStoreUpdate(update: StoreUpdate): void;
    onStoreError(code: string, op: StoreOp): void;
}

/** tick() 的结果，供日志与探针断言「这一轮到底发没发请求」。 */
export type TickOutcome = "queued" | "skipped-busy" | "skipped-paused";

/**
 * 由两份快照算出「这轮每家店到了几位客人」。
 *
 * 为什么可以做差：`shops[].visitors` 是**开业以来的历史累计**、跨业务日不清零
 * （唯一写入点 `internal/game/service.go:530`，跨日刷新只动 BusinessDay / CapFrozen /
 * DailyVisitorCap / VisitorsRemaining / LastAccrualAt），故差值恒为非负、跨日也成立。
 * 这里不碰任何规则常量，只做减法——口径登记见 `docs/M05-经营闭环.md` §2 D2。
 *
 * 没有基线（本会话首次快照）时返回空数组：宁可这一轮不出客人，也不按累计值一次性
 * 放出几十位——那正是《开发规划文档》§M05 风险条要避免的「看到人却对不上收益」。
 */
export function diffArrivals(prev: MallView | null, next: MallView): Arrival[] {
    if (!prev) return [];
    const arrivals: Arrival[] = [];
    for (const shop of next.shops) {
        const before = prev.shops.find((s) => s.id === shop.id);
        if (!before) {
            // 只有换了规则配置才会出现无基线的店铺 id：不猜，出声。
            console.warn(`[m05] 快照里出现无基线的店铺 ${shop.id}，本轮不计到店`);
            continue;
        }
        const delta = shop.visitors - before.visitors;
        if (delta > 0) {
            arrivals.push({ shopId: shop.id, visitors: delta });
        } else if (delta < 0) {
            // 累计值倒退只可能是换档或服务端异常：不生成客人，但必须留痕。
            console.error(`[m05] 店铺 ${shop.id} 累计客流倒退 ${before.visitors} → ${shop.visitors}，已忽略`);
        }
    }
    return arrivals;
}

/** 从未知异常里取稳定错误码；取不到就是 UNKNOWN，绝不吞掉异常本身。 */
function errorCode(err: unknown): string {
    if (typeof err === "object" && err !== null) {
        const code = (err as { code?: unknown }).code;
        if (typeof code === "string" && code.length > 0) return code;
    }
    return "UNKNOWN";
}

/**
 * 主界面的服务端状态编排器。生命周期由 MallScene 持有：
 * `load()` 一次 → `schedule(tick, 5)` → 前后台调 `pause()` / `resume()` → 销毁时停表。
 */
export class MallStore {
    /** 最后一次通过 revision 守卫并交给界面的快照，同时是 diff 的基线。 */
    private applied: MallView | null = null;
    /** 串行链：所有请求首尾相接，任何时刻最多一个在飞。 */
    private chain: Promise<void> = Promise.resolve();
    /** 已入队但未结束的请求数。tick 靠它决定「跳过」而不是「排队」。 */
    private inFlight = 0;
    private paused = false;
    private readonly transport: StoreTransport;
    private readonly observer: StoreObserver;

    constructor(transport: StoreTransport, observer: StoreObserver) {
        this.transport = transport;
        this.observer = observer;
    }

    /** 当前已应用的快照；启动取数完成前为 null。 */
    get view(): MallView | null {
        return this.applied;
    }

    get isPaused(): boolean {
        return this.paused;
    }

    get isBusy(): boolean {
        return this.inFlight > 0;
    }

    /**
     * 启动时取一次全量快照（B3.6「打开游戏：拉一次 /api/v1/mall」）。
     * 这一步只建立基线，通常不产生客人（没有上一份快照可做差）。
     */
    load(): Promise<void> {
        return this.enqueue("snapshot", async () => {
            const view = await this.transport.fetchMall();
            this.accept(view, null, "snapshot");
        });
    }

    /**
     * 每 5 秒一次的结算轮询。忙或后台时**跳过本轮而不排队**：
     * 排下去会让请求积压后集中结算，画面反而更难对齐到服务端节奏（§M05 风险条）。
     */
    tick(): TickOutcome {
        if (this.paused) {
            console.log("[m05] 轮询跳过：处于后台");
            return "skipped-paused";
        }
        if (this.inFlight > 0) {
            console.log(`[m05] 轮询跳过：上一个请求未结束（在飞 ${this.inFlight}）`);
            return "skipped-busy";
        }
        void this.enqueue("settle", async () => {
            const result = await this.transport.settle();
            this.accept(result.state, result, "settle");
        });
        return "queued";
    }

    /**
     * 开店。玩家点击必须入队而不是被 busy 拦掉——最多等一个在飞请求，
     * 串行链保证它不会与轮询交错。
     */
    prepare(shopId: string): Promise<void> {
        console.log(`[m05] 玩家请求开店：${shopId}`);
        return this.enqueue("prepare", async () => {
            const result = await this.transport.prepareShop(shopId);
            this.accept(result.state, result, "prepare");
        });
    }

    /** 切后台：停轮询。已在飞的请求照常收敛，数值仍以服务端为准。 */
    pause(): void {
        if (this.paused) return;
        this.paused = true;
        console.log("[m05] 进入后台：停止轮询");
    }

    /**
     * 回前台：立即取权威状态并恢复轮询（B3.6）。
     *
     * ⚠️ 这里发的是 **settle 而不是 GET /api/v1/mall**，与 B3.6 的字面写法不同：
     * `Service.Snapshot()`（`internal/game/service.go:254`）明确「不结算收益、不刷新业务日」，
     * 只拉快照的话后台期间的客流要等下一轮 tick 才算得出来，HUD 的业务日也可能停在上一天。
     * B3.6 括注要的正是「服务端会把这段时间的客流算出来」，而 settle 的返回体自带最新快照，
     * 一次请求即可满足。偏差已登记在 `docs/M05-经营闭环.md` §9。
     */
    resume(): Promise<void> {
        this.paused = false;
        console.log("[m05] 回到前台：立即取权威状态并恢复轮询");
        return this.enqueue("settle", async () => {
            const result = await this.transport.settle();
            this.accept(result.state, result, "settle");
        });
    }

    /** 入队一个请求。任务内部自行捕获异常，故串行链永不 reject。 */
    private enqueue(op: StoreOp, task: () => Promise<void>): Promise<void> {
        this.inFlight += 1;
        const run = this.chain.then(async () => {
            try {
                await task();
            } catch (err) {
                // 失败不清空界面、不打断节拍：界面保留最后一次成功值，下一轮照常。
                const code = errorCode(err);
                console.error(`[m05] ${op} 请求失败：${code}`, err);
                this.observer.onStoreError(code, op);
            } finally {
                this.inFlight -= 1;
            }
        });
        this.chain = run;
        return run;
    }

    /**
     * revision 单调守卫（验收第 3 条）：只接受比已应用值更高的 revision。
     * 相等 = 本轮空转（服务端没发布新状态），更小 = 乱序到达的旧响应，
     * 两种都丢弃——界面因此既不会回退，也不会把同一次到店播两遍。
     */
    private passesGuard(view: MallView): boolean {
        return this.applied === null || view.revision > this.applied.revision;
    }

    private accept(view: MallView, result: Result | null, op: StoreOp): void {
        if (!this.passesGuard(view)) {
            const applied = this.applied ? this.applied.revision : "无";
            console.log(`[m05] 丢弃 ${op} 响应：revision=${view.revision} 不高于已应用的 ${applied}`);
            return;
        }
        const arrivals = diffArrivals(this.applied, view);
        const shown = arrivals.reduce((sum, a) => sum + a.visitors, 0);
        if (result && shown !== result.visitorsUsed) {
            // 交叉校验：逐店做差的合计应当等于服务端报的本轮到店数。
            // 不等说明基线错位（中途换档、或漏应用过一次响应），必须出声而不是默默画错人数。
            console.warn(`[m05] 逐店到店合计 ${shown} 与服务端 visitorsUsed=${result.visitorsUsed} 不一致（op=${op}）`);
        }
        this.applied = view;
        console.log(
            `[m05] 应用 ${op} 响应 revision=${view.revision} changed=${result ? result.changed : "-"} ` +
            `金币=${view.coins} 已到店=${view.visitorsServed} 本轮到店=${shown}`,
        );
        this.observer.onStoreUpdate({
            view,
            arrivals,
            earnedCoins: result ? result.earnedCoins : 0,
            op,
        });
    }
}
