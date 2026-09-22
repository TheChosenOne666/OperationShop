// SC-M05-QA-002 独立回放探针：把 review-m05-http.mjs 从**真实后端**抓到的响应序列，
// 原样喂给被测的编排层 MallStore，再用复核方自己写的断言判它。
//
// 与 tools/m05_probe.ts 的区别就在数据源：这里的每一份快照都是服务端真发出来的，
// 不是夹具手搓的，所以「某状态不会发生」这类断言不会再是断言夹具本身。
//
// 用法：
//   node --experimental-strip-types tools/review-m05-replay.ts <captured.json>

import { readFileSync } from "node:fs";
import { MallStore } from "../NewProject/assets/scripts/MallStore.ts";
import type { StoreObserver, StoreTransport } from "../NewProject/assets/scripts/MallStore.ts";
import type { MallView, Result } from "../NewProject/assets/scripts/ApiTypes.ts";

const FILE = process.argv[2];
if (!FILE) {
    console.error("用法：node tools/review-m05-replay.ts <captured.json>");
    process.exit(2);
}
const captured = JSON.parse(readFileSync(FILE, "utf8"));
/** 真实交易序列：{ seq, atMs, method, path, status, body } */
const tx = captured.transcript.filter((e) => e.status === 200 && e.body);

/** 把真实响应按到达顺序装回三种 transport。 */
function buildTransport(): StoreTransport & { applied: number } {
    let i = 0;
    const next = (kind: string): { body: any } => {
        while (i < tx.length) {
            const e = tx[i++];
            const isGet = e.method === "GET" && e.path === "/api/v1/mall";
            const isSettle = e.path === "/api/v1/mall/settle";
            const isPrepare = /\/prepare$/.test(e.path);
            if (kind === "mall" && isGet) return e;
            if (kind === "settle" && isSettle) return e;
            if (kind === "prepare" && isPrepare) return e;
        }
        throw new Error(`${kind} 的真实响应已用完（共 ${tx.length} 条）`);
    };
    return {
        applied: 0,
        async fetchMall() { return next("mall").body as MallView; },
        async settle() { return next("settle").body as Result; },
        async prepareShop() { return next("prepare").body as Result; },
    };
}

const flush = () => new Promise<void>((r) => setTimeout(r, 5));

interface Applied {
    op: string;
    view: MallView;
    arrivals: Array<{ shopId: string; visitors: number }>;
    earnedCoins: number;
    /** 表现层会画的飘字总额：Σ 每位客人 ×「本次响应里该店的 unitPrice」（GuestStage.spawn 的算法）。 */
    floatedCoins: number;
}

class Collect implements StoreObserver {
    updates: Applied[] = [];
    failures: string[] = [];
    onStoreUpdate(u: any): void {
        let floated = 0;
        for (const a of u.arrivals) {
            const shop = u.view.shops.find((s: any) => s.id === a.shopId);
            floated += (shop ? shop.unitPrice : 0) * a.visitors;
        }
        this.updates.push({ op: u.op, view: u.view, arrivals: u.arrivals, earnedCoins: u.earnedCoins, floatedCoins: floated });
    }
    onStoreError(code: string): void { this.failures.push(code); }
}

const results: Array<{ name: string; pass: boolean; detail: string }> = [];
function ok(name: string, pass: boolean, detail = "") {
    results.push({ name, pass, detail });
    console.log(`${pass ? "PASS" : "FAIL"} | ${name}${detail ? " | " + detail : ""}`);
}

async function main() {
    console.log(`\n===== SC-M05-QA-002 真实响应回放（MallStore） @ ${FILE} =====`);
    // 按 transcript 顺序逐条驱动编排层：GET→load，settle→tick，prepare→prepare。
    const transport2 = buildTransport();
    const col2 = new Collect();
    const store2 = new MallStore(transport2, col2);
    await store2.load();
    for (const e of tx.slice(1)) {
        if (e.path === "/api/v1/mall/settle") { store2.tick(); await flush(); }
        else if (/\/prepare$/.test(e.path)) {
            // transport 按顺序取响应，不真的按 shopId 分派；这里只需触发 prepare 这条队列
            await store2.prepare("replay");
            await flush();
        }
    }

    const U = col2.updates;
    console.log(`回放驱动出 ${U.length} 次界面应用（真实响应共 ${tx.length} 条）`);
    U.forEach((u, i) => {
        const arr = u.arrivals.map((a) => `${a.shopId}×${a.visitors}`).join("+") || "—";
        console.log(`  #${i + 1} ${u.op.padEnd(8)} rev=${String(u.view.revision).padStart(3)} coins=${String(u.view.coins).padStart(5)} 已到店=${u.view.visitorsServed}/${u.view.dailyVisitorCap ?? "—"} 客人[${arr}] 服务端本轮+${u.earnedCoins} 飘字合计${u.floatedCoins}`);
    });

    // R1 验收 3：金币单调不减（不跳变不回退）
    let coinDrops: string[] = [];
    for (let i = 1; i < U.length; i += 1) {
        if (U[i].view.coins < U[i - 1].view.coins) coinDrops.push(`#${i}:${U[i - 1].view.coins}→${U[i].view.coins}`);
    }
    ok("R1 界面应用的金币序列单调不减（验收 3）", coinDrops.length === 0, coinDrops.join(" ") || `${U.length} 次应用`);

    // R2 revision 严格递增
    let revBad = 0;
    for (let i = 1; i < U.length; i += 1) if (U[i].view.revision <= U[i - 1].view.revision) revBad += 1;
    ok("R2 每次应用之间 revision 严格递增", revBad === 0, `违例 ${revBad}`);

    // R3 空转响应不得触发渲染
    const idleResponses = tx.filter((e) => e.path === "/api/v1/mall/settle" && e.body.changed === false).length;
    const appliedSettles = U.filter((u) => u.op === "settle").length;
    const settleResponses = tx.filter((e) => e.path === "/api/v1/mall/settle").length;
    ok("R3 空转 settle 一律不渲染", appliedSettles === settleResponses - idleResponses,
        `settle 响应 ${settleResponses} 条、其中空转 ${idleResponses} 条、界面只应用 ${appliedSettles} 次`);

    // R4 客人人数与服务端本轮到店数逐次一致
    const mism = [];
    for (let i = 0; i < U.length; i += 1) {
        const u = U[i];
        const shown = u.arrivals.reduce((s, a) => s + a.visitors, 0);
        if (u.op !== "snapshot") {
            // 用同一条响应体的 visitorsUsed 复核：找 revision 相同的 settle/prepare
            const src = tx.find((e) => e.body?.state?.revision === u.view.revision && e.body.visitorsUsed !== undefined);
            if (src && src.body.visitorsUsed !== shown) mism.push(`#${i + 1} 画${shown} 服务端${src.body.visitorsUsed}`);
        }
    }
    ok("R4 「画几位客人」与服务端 visitorsUsed 逐次一致", mism.length === 0, mism.join(" ") || "全部一致");

    // R5 客户端没有私改数值：每次应用的 view 与响应体逐字段相等
    let drift = 0;
    for (const u of U) {
        const src = tx.find((e) => e.body.state?.revision === u.view.revision || e.body.revision === u.view.revision);
        const body: MallView = src ? (src.body.state ?? src.body) : null;
        if (!body) { drift += 1; continue; }
        if (JSON.stringify(body) !== JSON.stringify(u.view)) drift += 1;
    }
    ok("R5 界面拿到的快照与真实响应逐字段相等（无私改）", drift === 0, `漂移 ${drift}/${U.length}`);

    // R6 飘字金额 vs 实际入账
    const floatsWrong = U.filter((u) => u.arrivals.length > 0 && u.op !== "snapshot" && u.floatedCoins !== u.earnedCoins);
    ok("R6 每次到店：Σ飘字(unitPrice) == 服务端 earnedCoins",
        floatsWrong.length === 0,
        floatsWrong.map((u) => `rev${u.view.revision}: 飘字 ${u.floatedCoins} vs 入账 ${u.earnedCoins}`).join("；") || `${U.filter((u) => u.arrivals.length > 0).length} 次到店全等`);

    // R7 未开业店不得出现在 arrivals（用真实数据）
    const shutHits = [];
    for (const u of U) {
        for (const a of u.arrivals) {
            const shop = u.view.shops.find((s) => s.id === a.shopId);
            if (shop && !shop.prepared) shutHits.push(`rev${u.view.revision} ${a.shopId}`);
        }
    }
    ok("R7 未开业铺位从未被画上人（真实响应）", shutHits.length === 0, shutHits.join(" ") || "0 次");

    const failed = results.filter((r) => !r.pass);
    console.log(`\n[replay] 通过 ${results.length - failed.length}/${results.length}`);
    if (failed.length) process.exitCode = 1;
}

void main();
