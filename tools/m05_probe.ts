// M05 编排层探针：用假 transport 驱动 MallStore，把「看不见但最容易错」的行为钉死。
//
// 为什么不用 jest：工程里没有前端测试框架，为一个模块引入新依赖不划算。
// MallStore.ts 刻意不 import "cc"，所以能直接用 Node 自带的类型擦除跑：
//     node --experimental-strip-types tools/m05_probe.ts
//
// 覆盖六类判据（对应 docs/M05-经营闭环.md §8 第 2 项）：
//   ① 逐店 diff 正确（含无基线、含 0 到店、含累计倒退、含未知店铺）
//   ② revision 单调守卫（相等/更小的响应被丢弃，界面不回退）
//   ③ 请求串行不并发（忙时 tick 跳过而非排队；开店排在在飞请求之后）
//   ④ 暂停期间不发请求，恢复时立即取权威状态
//   ⑤ 逐店合计与服务端 visitorsUsed 不一致时出声但仍应用
//   ⑥ 单个请求失败不断链，错误码上抛给界面层
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
function view(revision: number, coins: number, served: number, visitors: Record<string, number>): MallView {
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
        shops: Object.keys(visitors).map((id) => ({
            id,
            name: id,
            floor: 1,
            slot: 1,
            prepared: true,
            level: 1,
            unitPrice: 6,
            visitors: visitors[id] ?? 0,
            revenue: (visitors[id] ?? 0) * 6,
            upgradeCost: 300,
        })),
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
        assert.deepStrictEqual(diffArrivals(prev, next), [{ shopId: "coffee", visitors: 1 }]);
    });

    check("多店同时到店（回前台补算的形状）→ 逐店分别计数", () => {
        const prev = view(1, 1280, 10, { coffee: 5, flowers: 5 });
        const next = view(2, 1400, 18, { coffee: 9, flowers: 9 });
        assert.deepStrictEqual(diffArrivals(prev, next), [
            { shopId: "coffee", visitors: 4 },
            { shopId: "flowers", visitors: 4 },
        ]);
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

    check("快照里出现无基线的店铺 id → 跳过并出声", () => {
        const prev = view(1, 1280, 4, { coffee: 4 });
        const next = view(2, 1286, 5, { coffee: 5, bookstore: 1 });
        assert.deepStrictEqual(diffArrivals(prev, next), [{ shopId: "coffee", visitors: 1 }]);
        assert.ok(
            lastWarnings().some((line) => line.includes("无基线的店铺 bookstore")),
            "未知店铺必须留下 warn 日志",
        );
    });

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

    await checkAsync("revision 更小（乱序到达）→ 丢弃，金币不回退", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(9, 1500, 5, { coffee: 5 }));
        await store.load();
        transport.settleResults.push(result(view(8, 1280, 4, { coffee: 4 }), 1, 6));
        assert.strictEqual(store.tick(), "queued");
        await flush();
        assert.strictEqual(recorder.updates.length, 1);
        assert.strictEqual(store.view?.coins, 1500, "旧响应不得把金币拉回去");
        assert.strictEqual(store.view?.revision, 9);
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
        assert.deepStrictEqual(update.arrivals, [{ shopId: "coffee", visitors: 1 }]);
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
        assert.deepStrictEqual(update.arrivals, [{ shopId: "coffee", visitors: 3 }]);
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

    console.log("[m05-probe] ⑥ 验收第 1 条的数值形状");

    await checkAsync("开店 → 下一轮结算 → 一位客人归属该店、金币增加、已到店 +1", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        // 开局：一层两铺已解锁未开业，客流未冻结（dailyVisitorCap = null）
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0, flowers: 0 }));
        await store.load();
        assert.strictEqual(store.view?.dailyVisitorCap, null);

        // 玩家点开 f1-s1（咖啡）
        const afterPrepare = view(2, 1280, 0, { coffee: 0, flowers: 0 });
        afterPrepare.shops[0].prepared = true;
        transport.prepareResults.push(result(afterPrepare, 0, 0));
        await store.prepare("coffee");
        await flush();
        assert.strictEqual(recorder.updates.at(-1)?.op, "prepare");

        // 5 秒后第一轮结算：第一位客人到咖啡，当日上限就此冻结
        transport.settleResults.push(result(view(3, 1286, 1, { coffee: 1, flowers: 0 }), 1, 6));
        store.tick();
        await flush();
        const update = recorder.updates.at(-1);
        assert.deepStrictEqual(update?.arrivals, [{ shopId: "coffee", visitors: 1 }]);
        assert.strictEqual(update?.view.coins, 1286, "金币增加");
        assert.strictEqual(update?.view.visitorsServed, 1, "已到店 +1");
        assert.strictEqual(update?.view.dailyVisitorCap, 50, "首次实际产生客人的结算冻结当日上限");
        assert.strictEqual(update?.earnedCoins, 6);
    });

    console.log("[m05-probe] ⑦ 验收第 4 条的数值形状");

    await checkAsync("未开业铺位不出客人：只有已开业那家的 visitors 会动", async () => {
        const transport = new FakeTransport();
        const recorder = new Recorder();
        const store = new MallStore(transport, recorder);
        transport.mallResults.push(view(1, 1280, 0, { coffee: 0, flowers: 0 }));
        await store.load();
        // 连开三轮，客人按服务端轮转顺序只落到咖啡；花束始终未开业，累计恒为 0
        for (let round = 1; round <= 3; round += 1) {
            transport.settleResults.push(
                result(view(1 + round, 1280 + round * 6, round, { coffee: round, flowers: 0 }), 1, 6),
            );
            store.tick();
            await flush();
        }
        const all = recorder.updates.slice(1).flatMap((u) => u.arrivals);
        assert.deepStrictEqual(all, [
            { shopId: "coffee", visitors: 1 },
            { shopId: "coffee", visitors: 1 },
            { shopId: "coffee", visitors: 1 },
        ]);
        assert.ok(
            all.every((a) => a.shopId !== "flowers"),
            "未开业的铺位不得产生客人",
        );
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
