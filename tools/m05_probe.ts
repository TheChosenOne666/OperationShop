// M05 编排层探针：用假 transport 驱动 MallStore，把「看不见但最容易错」的行为钉死。
//
// 为什么不用 jest：工程里没有前端测试框架，为一个模块引入新依赖不划算。
// MallStore.ts 刻意不 import "cc"，所以能直接用 Node 自带的类型擦除跑：
//     node --experimental-strip-types tools/m05_probe.ts
//
// 覆盖七类判据（对应 docs/M05-经营闭环.md §8 第 2 项）：
//   ① 逐店 diff 正确（含无基线、含 0 到店、含累计倒退告警、含**单价取上一份快照**）
//   ② revision 守卫：相等（空转）丢弃；变小判为服务端重建档 → 清基线重新应用
//   ③ 请求串行不并发；界面回调抛异常既不投毒链、也不提交基线
//   ④ 暂停期间不发请求，恢复时立即取权威状态
//   ⑤ 逐店合计与服务端 visitorsUsed 不一致时出声但仍应用；单个请求失败不断链、错误码上抛
//   ⑥ 开店 → 首笔收益这一串在编排层交出的数值形状
//   ⑦ diff 只收累计值有增量的店
// ⚠️ ⑥⑦ 的组名刻意带上「不证验收 N」：它们跑的是假 transport，不是《开发规划文档》§三
//    那四条验收标准的证据（独立复核 SC-M05-QA-002 P2-B）。
// ⚠️ 边界要说清：本探针证的是**编排层**。S2→S3 换图、客人真走到门口、
//    以及「未开业不产收益」这条服务端不变量，分别归实机截图与 internal/game 的 Go 测试。
//    （独立复核 SC-M05-QA-001 P2 指出：夹具原先把 prepared 一律写死为 true，
//     使"开店""未开业不出客人"两条变成断言自己造的假数据——已改为显式声明；
//     SC-M05-QA-002 §3a 进一步指出 prepared 对本层的 diff 根本不可读，
//     故 ⑦ 改用「未开业店累计值取非零常量」让那条断言真的可能失败。）
import assert from "node:assert";
import { diffArrivals, MallStore } from "../NewProject/assets/scripts/MallStore.ts";
import type { Arrival, StoreOp, StoreObserver, StoreTransport, StoreUpdate } from "../NewProject/assets/scripts/MallStore.ts";
import type { MallView, Result } from "../NewProject/assets/scripts/ApiTypes.ts";

// ---------- 控制台留痕：既打印（作为证据）又收集（供断言） ----------

const warnings: string[] = [];
const errors: string[] = [];
const originalWarn = console.warn;
const originalError = console.error;
console.warn = (...args: unknown[]) => {
    warnings.push(args.map(String).join(" "));
    originalWarn(...args);
};
console.error = (...args: unknown[]) => {
    errors.push(args.map(String).join(" "));
    originalError(...args);
};

// ---------- 构造假数据 ----------

/** 按「店铺 id → 累计客流」造一份快照。其余字段填成与真服务端同形的值。 */
/**
 * 按「店铺 id → 累计客流」造一份快照。
 * `prepared` 显式给出**已开业**的店铺集合；省略时视为全部已开业。
 * ⚠️ 不能一律写死 `prepared: true`——那会让"开店"与"未开业不出客人"两类用例变成
 *    断言自己造的假数据（独立复核 SC-M05-QA-001 P2 抓到的正是这条）。
 * `prices` 覆盖各店单价（省略即 6）；SC-M05-QA-002 P1-A 的那条用例要用到"同一轮里价变了"。
 */
function view(
    revision: number,
    coins: number,
    served: number,
    visitors: Record<string, number>,
    prepared?: string[],
    prices?: Record<string, number>,
): MallView {
    return {
        schemaVersion: 2,
        rulesFingerprint: "probe",
        revision,
        coins,
        spent: 0,
        businessDay: "2026-09-21",
        capFrozen: served > 0,
        dailyVisitorCap: served > 0 ? 50 : null,
        visitorsServed: served,
        visitorsRemaining: served > 0 ? 50 - served : 0,
        lastAccrualAt: "2026-09-21T00:00:00Z",
        lastObservedAt: "2026-09-21T00:00:00Z",
        unlockedSlots: ["f1-s1", "f1-s2"],
        nextSlotId: "f2-s1",
        nextUnlockCost: 600,
        shops: Object.keys(visitors).map((id) => {
            const price = prices?.[id] ?? 6;
            return {
                id,
                name: id,
                floor: 1,
                slot: 1,
                prepared: prepared ? prepared.includes(id) : true,
                level: 1,
                unitPrice: price,
                visitors: visitors[id] ?? 0,
                revenue: (visitors[id] ?? 0) * price,
                upgradeCost: 300,
            };
        }),
    };
}

function result(state: MallView, visitorsUsed: number, earnedCoins: number, changed = true): Result {
    return { state, earnedCoins, visitorsUsed, changed };
}

/** 造一个带稳定错误码的异常，形状与 ApiClient 的 ApiError 一致。 */
function apiError(code: string): Error {
    return Object.assign(new Error(code), { code });
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
    let resolve: () => void = () => undefined;
    const promise = new Promise<void>((r) => {
        resolve = r;
    });
    return { promise, resolve };
}

/** 让出一轮事件循环，把 enqueue 链上的微任务全部冲完。 */
const flush = () => new Promise<void>((r) => setTimeout(r, 0));

class FakeTransport implements StoreTransport {
    readonly calls: string[] = [];
    mallResults: Array<MallView | Error> = [];
    settleResults: Array<Result | Error> = [];
    prepareResults: Array<Result | Error> = [];
    /** 挂住请求用：非空时每个请求都要先等它，用来制造「在飞」状态。 */
    gate: Promise<void> | null = null;

    async fetchMall(): Promise<MallView> {
        this.calls.push("mall");
        if (this.gate) await this.gate;
        return take(this.mallResults);
    }

    async settle(): Promise<Result> {
        this.calls.push("settle");
        if (this.gate) await this.gate;
        return take(this.settleResults);
    }

    async prepareShop(shopId: string): Promise<Result> {
        this.calls.push(`prepare:${shopId}`);
        if (this.gate) await this.gate;
        return take(this.prepareResults);
    }
}

function take<T>(queue: Array<T | Error>): T {
    const item = queue.shift();
    if (item === undefined) throw new Error("假 transport 被调用次数超出预设响应");
    if (item instanceof Error) throw item;
    return item as T;
}

class Recorder implements StoreObserver {
    readonly updates: StoreUpdate[] = [];
    readonly failures: Array<{ code: string; op: StoreOp }> = [];

    onStoreUpdate(update: StoreUpdate): void {
        this.updates.push(update);
    }

    onStoreError(code: string, op: StoreOp): void {
        this.failures.push({ code, op });
    }
}

// ---------- 断言外壳 ----------

let passed = 0;
const failures: string[] = [];

function check(name: string, run: () => void): void {
    const warnBefore = warnings.length;
    const errorBefore = errors.length;
    try {
        run();
        passed += 1;
        console.log(`  ✓ ${name}`);
    } catch (err) {
        failures.push(name);
        console.log(`  ✗ ${name}`);
        console.log(`      ${(err as Error).message}`);
    }
    // 每条用例只对自己的日志负责，避免串味
    warnings.splice(warnBefore, warnings.length - warnBefore);
    errors.splice(errorBefore, errors.length - errorBefore);
}

async function checkAsync(name: string, run: () => Promise<void>): Promise<void> {
    const warnBefore = warnings.length;
    const errorBefore = errors.length;
    try {
        await run();
        passed += 1;
        console.log(`  ✓ ${name}`);
    } catch (err) {
        failures.push(name);
        console.log(`  ✗ ${name}`);
        console.log(`      ${(err as Error).message}`);
    }
    warnings.splice(warnBefore, warnings.length - warnBefore);
    errors.splice(errorBefore, errors.length - errorBefore);
}

function lastWarnings(): string[] {
    return warnings.slice();
}

function lastErrors(): string[] {
    return errors.slice();
}

function arrivalsOf(update: StoreUpdate | undefined): Arrival[] {
    return update ? update.arrivals : [];
}

// ---------- 用例 ----------

async function main(): Promise<void> {
    console.log("[m05-probe] ① 逐店 diff");

    check("无基线（本会话首份快照）→ 不产生客人", () => {
        const next = view(1, 1280, 0, { coffee: 12 });
        assert.deepStrictEqual(diffArrivals(null, next), []);
    });

    check("单店 +1 → 一位客人归属该店", () => {
        const prev = view(1, 1280, 3, { coffee: 3, flowers: 0 });
        const next = view(2, 1286, 4, { coffee: 4, flowers: 0 });
        assert.deepStrictEqual(diffArrivals(prev, next), [{ shopId: "coffee", visitors: 1, unitPrice: 6 }]);
    });

    check("多店同时到店（回前台补算的形状）→ 逐店分别计数", () => {
        const prev = view(1, 1280, 10, { coffee: 5, flowers: 5 });
        const next = view(2, 1400, 18, { coffee: 9, flowers: 9 });
        assert.deepStrictEqual(diffArrivals(prev, next), [
            { shopId: "coffee", visitors: 4, unitPrice: 6 },
            { shopId: "flowers", visitors: 4, unitPrice: 6 },
        ]);
    });

    // SC-M05-QA-002 P1-A：服务端 mutate 先 accrue 后 apply，所以**同一条 Prepare 响应**里
    // 客人是按旧价入账的，而 view().unitPrice 已是新价。飘字若取本次响应的价，就会显示
    // 服务端没收的钱（实测界面飘 +7、实际入账 6）。arrivals 的单价必须来自上一份快照。
    check("本轮客人按**上一份快照**的单价入账，不用本次响应里涨上去的价", () => {
        const prev = view(1, 1280, 3, { coffee: 3 }, ["coffee"], { coffee: 6 });
        const next = view(2, 1286, 4, { coffee: 4 }, ["coffee"], { coffee: 7 });
        assert.deepStrictEqual(diffArrivals(prev, next), [{ shopId: "coffee", visitors: 1, unitPrice: 6 }]);
        assert.strictEqual(next.shops[0].unitPrice, 7, "夹具本身要让「本次响应里已是新价」成立，否则这条在断言空气");
    });

    check("0 到店（空闲轮询）→ 空数组", () => {
        const prev = view(1, 1280, 4, { coffee: 4 });
        const next = view(1, 1280, 4, { coffee: 4 });
        assert.deepStrictEqual(diffArrivals(prev, next), []);
    });

    check("累计客流倒退 → 忽略并出声", () => {
        const prev = view(1, 1280, 4, { coffee: 4 });
        const next = view(2, 1280, 4, { coffee: 2 });
        assert.deepStrictEqual(diffArrivals(prev, next), []);
        assert.ok(
            lastErrors().some((line) => line.includes("累计客流倒退")),
            "倒退必须留下 error 日志，不能静默",
        );
    });

    // 注：原先这里有一条「快照里出现无基线的店铺 id → 出声」的用例，已随实现一并删掉。
    // 同一会话内店铺清单由服务端配置固定、不会增减（改配置会改规则指纹→服务端拒旧档），
    // 那条分支在真实协议下不可达，留着就是拿探针给它"造一个成立的现场"。
    // 独立复核 SC-M05-QA-001 P2；与 §八-10 去死代码是同一个判断。

    console.log("[m05-probe] ② revision 单调守卫");

    await checkAsync("启动 load 走 GET /api/v1/mall，建立基线", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0, flowers: 0 }));
        const store = new MallStore(transport, recorder);
        await store.load();
        assert.deepStrictEqual(transport.calls, ["mall"]);
        assert.strictEqual(recorder.updates.length, 1);
        assert.strictEqual(store.view?.revision, 1);
        assert.deepStrictEqual(arrivalsOf(recorder.updates[0]), [], "首份快照没有基线，不该凭空出客人");
    });

    await checkAsync("revision 相等（空转）→ 丢弃，界面不动", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(7, 1280, 0, { coffee: 0 }));
        await store.load();
        transport.settleResults.push(result(view(7, 1280, 0, { coffee: 0 }), 0, 0, false));
        assert.strictEqual(store.tick(), "queued");
        await flush();
        assert.strictEqual(recorder.updates.length, 1, "空转响应不得触发第二次渲染");
        assert.ok(lastWarnings().length === 0, "空转是常态，不该告警");
    });

    await checkAsync("revision 变小 ⇒ 判为服务端重建档，清基线重新应用（不永久冻结）", async () => {
        // 本设计请求严格串行、同时只有一个在飞，**不存在"乱序到达的旧响应"**；
        // 所以 revision 变小只可能是删档重建 / 改配置拒旧档（服务端从 1 重新计数）。
        // 原用例把它当乱序丢弃并断言"金币保持 1500"，恰好把这条真实故障写成了期望值
        // ——独立复核 SC-M05-QA-001 P1-2。
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(9, 1500, 5, { coffee: 5 }));
        await store.load();
        const rebuilt = view(1, 1280, 0, { coffee: 0 });
        rebuilt.rulesFingerprint = "m05-v3";
        transport.settleResults.push(result(rebuilt, 0, 0));
        assert.strictEqual(store.tick(), "queued");
        await flush();
        assert.strictEqual(recorder.updates.length, 2, "基线作废后必须重新应用，否则界面永久停在旧值");
        assert.strictEqual(store.view?.revision, 1);
        assert.strictEqual(store.view?.coins, 1280);
        assert.deepStrictEqual(arrivalsOf(recorder.updates[1]), [], "新基线不做差，不凭累计值放客人");
        assert.ok(
            lastWarnings().some((line) => line.includes("基线作废")),
            "换档是异常事件，必须留痕",
        );
    });

    await checkAsync("界面回调抛异常不投毒串行链，且不提交基线", async () => {
        // observer 是在 catch/accept 里被调的用户代码。它抛一次若让链 reject，
        // 后续每个任务体都不再执行、inFlight 只增不减，轮询会静默永久跳过（P1-1）。
        const transport = new FakeTransport();
        const seen: StoreUpdate[] = [];
        let blowUp = true;
        const store = new MallStore(transport, {
            onStoreUpdate: (update) => {
                if (blowUp) {
                    blowUp = false;
                    throw new Error("render blew up");
                }
                seen.push(update);
            },
            onStoreError: () => undefined,
        });
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        assert.strictEqual(seen.length, 0);
        assert.strictEqual(store.view, null, "渲染失败的响应不得提交基线");
        assert.strictEqual(store.isBusy, false, "在飞计数必须归零");
        transport.settleResults.push(result(view(2, 1286, 1, { coffee: 1 }), 1, 6));
        assert.strictEqual(store.tick(), "queued", "链被投毒的话这里会一直 skipped-busy");
        await flush();
        assert.strictEqual(seen.length, 1, "下一轮必须照常送达界面");
        assert.strictEqual(seen[0].view.coins, 1286);
    });

    await checkAsync("revision 更大 → 应用，数值与 arrivals 一并交出", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        transport.settleResults.push(result(view(2, 1286, 1, { coffee: 1 }), 1, 6));
        await flush();
        assert.strictEqual(store.tick(), "queued");
        await flush();
        assert.strictEqual(recorder.updates.length, 2);
        const update = recorder.updates[1];
        assert.strictEqual(update.view.coins, 1286);
        assert.strictEqual(update.earnedCoins, 6);
        assert.deepStrictEqual(update.arrivals, [{ shopId: "coffee", visitors: 1, unitPrice: 6 }]);
    });

    console.log("[m05-probe] ③ 串行与竞态");

    await checkAsync("请求在飞时 tick 跳过（不排队、不并发）", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const gate = deferred();
        transport.gate = gate.promise;
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        void store.load();
        await flush();
        assert.strictEqual(store.isBusy, true, "load 被 gate 挂住，应当处于在飞状态");
        transport.settleResults.push(result(view(2, 1286, 1, { coffee: 1 }), 1, 6));
        assert.strictEqual(store.tick(), "skipped-busy");
        assert.strictEqual(store.tick(), "skipped-busy");
        assert.deepStrictEqual(transport.calls, ["mall"], "跳过就是不发请求，calls 不该增长");
        gate.resolve();
        await flush();
        assert.strictEqual(store.isBusy, false);
    });

    await checkAsync("开店排在在飞的结算之后，两者不交错", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        // 基线建好后才挂 gate：这一轮的 settle 会停在半路，制造「在飞」状态
        const gate = deferred();
        transport.gate = gate.promise;
        // 空转响应：revision 不变，按守卫应被丢弃
        transport.settleResults.push(result(view(1, 1280, 0, { coffee: 0 }), 0, 0, false));
        transport.prepareResults.push(result(view(2, 1280, 0, { coffee: 0 }), 0, 0));
        assert.strictEqual(store.tick(), "queued");
        await flush();
        assert.strictEqual(store.tick(), "skipped-busy", "settle 仍在飞");
        void store.prepare("coffee");
        await flush();
        assert.deepStrictEqual(transport.calls, ["mall", "settle"], "gate 未开，prepare 还排着");
        gate.resolve();
        await flush();
        assert.deepStrictEqual(
            transport.calls,
            ["mall", "settle", "prepare:coffee"],
            "三个请求必须严格按入队顺序发出",
        );
        assert.strictEqual(recorder.updates.length, 2, "settle 空转被丢弃，只有 load 与 prepare 触发渲染");
        assert.strictEqual(recorder.updates[1].op, "prepare");
    });

    console.log("[m05-probe] ④ 前后台");

    await checkAsync("暂停期间 tick 不发请求", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        store.pause();
        assert.strictEqual(store.isPaused, true);
        assert.strictEqual(store.tick(), "skipped-paused");
        assert.strictEqual(store.tick(), "skipped-paused");
        assert.deepStrictEqual(transport.calls, ["mall"]);
    });

    await checkAsync("恢复时立即发一次 settle（自带快照并补算后台期间客流）", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        store.pause();
        // 后台 10 分钟：服务端一次性算出 3 位客人
        transport.settleResults.push(result(view(2, 1298, 3, { coffee: 3 }), 3, 18));
        await store.resume();
        await flush();
        assert.deepStrictEqual(transport.calls, ["mall", "settle"]);
        assert.strictEqual(store.isPaused, false);
        const update = recorder.updates[1];
        assert.deepStrictEqual(update.arrivals, [{ shopId: "coffee", visitors: 3, unitPrice: 6 }]);
        assert.strictEqual(update.view.coins, 1298);
    });

    console.log("[m05-probe] ⑤ 交叉校验与失败恢复");

    await checkAsync("逐店合计与服务端 visitorsUsed 不一致 → 仍应用但告警", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        // 逐店只多出 1 位，服务端却报 2 位：基线错位，必须出声
        transport.settleResults.push(result(view(2, 1292, 2, { coffee: 1 }), 2, 12));
        await flush();
        store.tick();
        await flush();
        assert.strictEqual(recorder.updates.length, 2, "不一致不等于不可信，数值仍以服务端为准");
        assert.ok(
            lastWarnings().some((line) => line.includes("不一致")),
            "必须留下不一致告警",
        );
    });

    await checkAsync("请求失败 → 错误码上抛、界面保留旧值、节拍不断", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        transport.settleResults.push(apiError("NETWORK"));
        store.tick();
        await flush();
        assert.deepStrictEqual(recorder.failures, [{ code: "NETWORK", op: "settle" }]);
        assert.strictEqual(store.view?.coins, 1280, "失败不得清空或回退界面数值");
        assert.strictEqual(recorder.updates.length, 1, "失败不触发渲染");
        // 链没断：下一轮照常发请求并成功
        transport.settleResults.push(result(view(2, 1286, 1, { coffee: 1 }), 1, 6));
        assert.strictEqual(store.tick(), "queued");
        await flush();
        assert.strictEqual(recorder.updates.length, 2);
        assert.strictEqual(store.view?.coins, 1286);
        assert.strictEqual(store.isBusy, false, "失败后在飞计数必须归零，否则轮询会永久跳过");
    });

    await checkAsync("开店失败（铺位未解锁）→ 409 SLOT_LOCKED 上抛，不改动基线", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0 }));
        await store.load();
        transport.prepareResults.push(apiError("SLOT_LOCKED"));
        await store.prepare("clothing");
        await flush();
        assert.deepStrictEqual(recorder.failures, [{ code: "SLOT_LOCKED", op: "prepare" }]);
        assert.strictEqual(store.view?.revision, 1);
        assert.strictEqual(recorder.updates.length, 1);
    });

    console.log("[m05-probe] ⑥ 编排层交出的数值形状（不证验收 1）");

    await checkAsync("（仅编排层，不证验收第 1 条）开店 → 下一轮结算：arrivals 归属该店、金币与已到店各 +1", async () => {
        // ⚠️ 用例名里带「开店 / 金币增加 / 已到店 +1」，但它**不是验收第 1 条的证据**：
        //    跑的是假 transport，S2→S3 换图、客人真的走到门口都不在这里。
        //    独立复核 SC-M05-QA-002 P2-B 要求把这层关系写进名字，否则只看名字会误记"验收已过"。
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        // 开局：一层两铺已解锁但**都未开业**，客流未冻结（dailyVisitorCap = null）
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0, flowers: 0 }, []));
        await store.load();
        assert.strictEqual(store.view?.dailyVisitorCap, null);
        assert.ok(
            store.view?.shops.every((s) => !s.prepared),
            "开局夹具里两家店必须都是未开业，否则'开店'这一步是假的",
        );

        // 玩家点开 f1-s1（咖啡）：只有咖啡变已开业
        transport.prepareResults.push(result(view(2, 1280, 0, { coffee: 0, flowers: 0 }, ["coffee"]), 0, 0));
        await store.prepare("coffee");
        await flush();
        assert.strictEqual(recorder.updates.at(-1)?.op, "prepare");
        assert.strictEqual(recorder.updates.at(-1)?.view.shops.find((s) => s.id === "coffee")?.prepared, true);

        // 5 秒后第一轮结算：第一位客人到咖啡，当日上限就此冻结
        transport.settleResults.push(
            result(view(3, 1286, 1, { coffee: 1, flowers: 0 }, ["coffee"]), 1, 6),
        );
        store.tick();
        await flush();
        const update = recorder.updates.at(-1);
        assert.deepStrictEqual(update?.arrivals, [{ shopId: "coffee", visitors: 1, unitPrice: 6 }]);
        assert.strictEqual(update?.view.coins, 1286, "金币增加");
        assert.strictEqual(update?.view.visitorsServed, 1, "已到店 +1");
        assert.strictEqual(update?.view.dailyVisitorCap, 50, "首次实际产生客人的结算冻结当日上限");
        assert.strictEqual(update?.earnedCoins, 6);
    });

    console.log("[m05-probe] ⑦ 编排层的 diff 过滤（不证验收 4）");

    await checkAsync("（仅编排层，不证验收第 4 条）累计值不变的店不会出现在 arrivals 里", async () => {
        // 口径要说清：验收第 4 条「未开业铺位不产生收益、不消耗客流」是**服务端不变量**
        //（accrue 跳过未开业店），由 internal/game 的 Go 测试保证，探针证不了它。
        // 探针能证的只有客户端这一半：diffArrivals 只收 delta>0 的店。
        // ⚠️ 夹具里未开业店的累计值**不能写 0**：写 0 的话"它没出现在 arrivals 里"是夹具
        //    自己造成的，任何实现都能通过（SC-M05-QA-002 §3a 指出 84b2fa6 没改掉这个根，
        //    因为 diffArrivals 与 MallStore 根本不读 prepared 字段）。这里给花束一个非零常量。
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 3, { coffee: 0, flowers: 5 }, ["coffee"]));
        await store.load();
        assert.strictEqual(
            store.view?.shops.find((s) => s.id === "flowers")?.visitors,
            5,
            "花束的累计值必须是非零常量，否则下面「它不在 arrivals 里」又是断言假数据",
        );
        // 连开三轮，客人按服务端轮转顺序只落到咖啡；花束这一会话内一个客人没来
        for (let round = 1; round <= 3; round += 1) {
            transport.settleResults.push(
                result(view(1 + round, 1280 + round * 6, round, { coffee: round, flowers: 5 }, ["coffee"]), 1, 6),
            );
            store.tick();
            await flush();
        }
        const all = recorder.updates.slice(1).flatMap((u) => u.arrivals);
        assert.deepStrictEqual(all, [
            { shopId: "coffee", visitors: 1, unitPrice: 6 },
            { shopId: "coffee", visitors: 1, unitPrice: 6 },
            { shopId: "coffee", visitors: 1, unitPrice: 6 },
        ]);
        assert.strictEqual(store.view?.coins, 1298);
        assert.strictEqual(store.view?.visitorsServed, 3);
    });

    // ---------- 汇总 ----------

    console.log("");
    if (failures.length === 0) {
        console.log(`[m05-probe] 全部通过：${passed} 项`);
        return;
    }
    console.log(`[m05-probe] 通过 ${passed} 项，失败 ${failures.length} 项：`);
    for (const name of failures) console.log(`  - ${name}`);
    process.exitCode = 1;
}

void main();
