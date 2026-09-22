// SC-M05-QA-002 独立复核探针（复核方自写，不复用 tools/m05_probe.ts 的任何判据）。
//
// 只做一件事：起真实进程、走真实 HTTP，把 M05 四条验收标准的**服务端侧**证据自己取出来。
// 探针不 import 客户端代码，因此不存在"用实现者的假设验实现者"的问题。
// 客户端侧由 tools/review-m05-replay.ts 用这里产出的真实响应序列回放验证。
//
// 用法（需先起后端）：
//   MALL_DEV_TOKEN 需与后端一致，脚本从环境变量读：
//   node tools/review-m05-http.mjs <baseUrl> <outJson>
// 例：
//   node tools/review-m05-http.mjs http://127.0.0.1:18091 /tmp/sc-m05-qa/review-http.json
import { writeFileSync } from "node:fs";

const BASE = process.argv[2] || "http://127.0.0.1:18091";
const OUT = process.argv[3] || "review-m05-http.json";
const TOKEN = process.env.MALL_DEV_TOKEN;
if (!TOKEN) {
    console.error("必须设 MALL_DEV_TOKEN 环境变量（与后端同一份）");
    process.exit(2);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const transcript = [];
const results = [];
let seq = 0;

async function call(method, path, body) {
    const t = Date.now();
    const res = await fetch(BASE + path, {
        method,
        headers: { Authorization: `Bearer ${TOKEN}`, "Content-Type": "application/json" },
        ...(method === "GET" ? {} : { body: "{}" }),
    });
    const json = await res.json().catch(() => null);
    const entry = { seq: ++seq, atMs: t, method, path, status: res.status, body: json };
    transcript.push(entry);
    return json;
}

function ok(name, pass, detail) {
    results.push({ name, pass, detail });
    console.log(`${pass ? "PASS" : "FAIL"} | ${name}${detail ? " | " + detail : ""}`);
}

/** 金币、已到店、上限、逐店累计客流——所有断言只看这几项。 */
function digest(view) {
    return {
        revision: view.revision,
        coins: view.coins,
        capFrozen: view.capFrozen,
        dailyVisitorCap: view.dailyVisitorCap,
        visitorsServed: view.visitorsServed,
        visitorsRemaining: view.visitorsRemaining,
        shops: view.shops.map((s) => `${s.id}:${s.prepared ? "open" : "shut"}/v${s.visitors}/$${s.revenue}@${s.unitPrice}`),
    };
}

async function main() {
    console.log(`\n===== SC-M05-QA-002 真实 HTTP 探针 @ ${BASE} =====`);

    // ---------- 1. 冷启动 ----------
    const cold = await call("GET", "/api/v1/mall");
    console.log("[1] 冷启动快照", JSON.stringify(digest(cold)));
    ok("1-a 冷启动金币取服务端值", cold.coins === 1280, `coins=${cold.coins}`);
    ok("1-b 未冻结时 dailyVisitorCap 为 null（GDD §6.1）", cold.capFrozen === false && cold.dailyVisitorCap === null,
        `capFrozen=${cold.capFrozen} cap=${JSON.stringify(cold.dailyVisitorCap)}`);
    ok("1-c 无已开业店铺", cold.shops.every((s) => !s.prepared), cold.shops.map((s) => s.id + ":" + s.prepared).join(","));
    ok("1-d 未开业店累计客流与收益均为 0（验收 4 的起点）",
        cold.shops.every((s) => s.visitors === 0 && s.revenue === 0), "");

    // 0 店开业的空转结算：不落盘、revision 不变（§八-9 已裁语义）
    const idle1 = await call("POST", "/api/v1/mall/settle");
    await sleep(1200);
    const idle2 = await call("POST", "/api/v1/mall/settle");
    ok("1-e 0 店空转结算 changed=false 且 revision 不变",
        idle1.changed === false && idle2.changed === false && idle1.state.revision === cold.revision && idle2.state.revision === cold.revision,
        `rev ${cold.revision}→${idle1.state.revision}→${idle2.state.revision} changed=${idle1.changed}/${idle2.changed}`);
    ok("1-f 0 店空转不产出客流", idle1.visitorsUsed === 0 && idle2.visitorsUsed === 0, "");

    // GET /api/v1/mall 不结算——这条是「回前台到底该发 GET 还是 settle」的判断依据
    const beforeLazy = await call("GET", "/api/v1/mall");
    await sleep(6200);
    const afterLazy = await call("GET", "/api/v1/mall");
    ok("1-g GET 快照不推进结算（故 B3.6 字面的「回前台拉快照」算不出后台客流）",
        afterLazy.revision === beforeLazy.revision && afterLazy.coins === beforeLazy.coins,
        `revision ${beforeLazy.revision}→${afterLazy.revision}`);

    // ---------- 2. 验收 1：开店到第一位客人到店的真实耗时 ----------
    const t0 = Date.now();
    const prep = await call("POST", "/api/v1/shops/coffee/prepare");
    ok("2-a 开店返回 changed=true", prep.changed === true,
        `rev ${prep.state.revision} coffee=${prep.state.shops[0].prepared}`);
    ok("2-b 开店本身不产收益（首次开店以准备时刻起算）",
        prep.visitorsUsed === 0 && prep.earnedCoins === 0, `used=${prep.visitorsUsed} earned=${prep.earnedCoins}`);
    ok("2-c 开店后上限仍未冻结", prep.state.dailyVisitorCap === null, JSON.stringify(prep.state.dailyVisitorCap));

    let first = null;
    const pollLog = [];
    for (let i = 0; i < 40; i += 1) {
        await sleep(500);
        const r = await call("POST", "/api/v1/mall/settle");
        const dt = (Date.now() - t0) / 1000;
        pollLog.push(`${dt.toFixed(2)}s:used=${r.visitorsUsed},rev=${r.state.revision},coins=${r.state.coins},served=${r.state.visitorsServed},cap=${JSON.stringify(r.state.dailyVisitorCap)}`);
        if (r.visitorsUsed >= 1) { first = { ...r, dt }; break; }
    }
    console.log("[2] 轮询轨迹\n    " + pollLog.join("\n    "));
    ok("2-d 开店后出现第一位客人", !!first, first ? `Δ=${first.dt.toFixed(2)}s` : "超时未出现");
    if (first) {
        ok("2-e 金币按服务端单价增加（验收 1）", first.state.coins === 1286 && first.earnedCoins === 6,
            `coins=${first.state.coins} earned=${first.earnedCoins}`);
        ok("2-f 客流「已到店」+1 且上限就此冻结（HUD 契约 §6.1）",
            first.state.visitorsServed === 1 && first.state.capFrozen === true && first.state.dailyVisitorCap === 35,
            `served=${first.state.visitorsServed} cap=${first.state.dailyVisitorCap}`);
        ok("2-g visitorsRemaining 相应 −1", first.state.visitorsRemaining === 34, `${first.state.visitorsRemaining}`);
        ok("2-h 到店归属只落在已开业的咖啡",
            first.state.shops.find((s) => s.id === "coffee").visitors === 1 &&
            first.state.shops.find((s) => s.id === "flowers").visitors === 0, "");
        ok("2-i 「5 秒内」的字面判据", first.dt <= 5.0,
            `实测第一位客人出现在开店后 ${first.dt.toFixed(2)}s（轮询与结算间隔各 5s，相位叠加）`);
        globalThis.__first = first;
    }

    // ---------- 3. 验收 4：只开一家时，另一家既不出收益也不消耗客流 ----------
    let shutRounds = 0;
    for (let i = 0; i < 3; i += 1) {
        await sleep(5200);
        const r = await call("POST", "/api/v1/mall/settle");
        const f = r.state.shops.find((s) => s.id === "flowers");
        const c = r.state.shops.find((s) => s.id === "coffee");
        if (f.visitors === 0 && f.revenue === 0 && f.unitPrice === 9) shutRounds += 1;
        console.log(`[3] 轮 ${i}: coffee v=${c.visitors} $=${c.revenue} rev=${r.state.revision} 已到店=${r.state.visitorsServed} 剩=${r.state.visitorsRemaining}`);
    }
    ok("3-a 未开业铺位连续三轮零客流零收益", shutRounds === 3, `${shutRounds}/3`);
    const idleCheck = await call("GET", "/api/v1/mall");
    ok("3-b 未开业的花束单价仍为等级价（无满铺加成）",
        idleCheck.shops.find((s) => s.id === "flowers").unitPrice === 9, `${idleCheck.shops.find((s) => s.id === "flowers").unitPrice}`);

    // ---------- 4. 争议点 c：Prepare 响应里「本轮客人」与「响应单价」的取值时机 ----------
    console.log("\n[4] 制造「本轮有客人 + 同一响应里单价变化」的现场");
    await sleep(6200);           // 让一位客人处于待结算状态（客户端这段时间不轮询 = 后台场景）
    const coinsBefore = (await call("GET", "/api/v1/mall")).coins;
    const priceBefore = (await call("GET", "/api/v1/mall")).shops.find((s) => s.id === "coffee").unitPrice;
    const prep2 = await call("POST", "/api/v1/shops/flowers/prepare");
    const priceAfter = prep2.state.shops.find((s) => s.id === "coffee").unitPrice;
    const priceAfterFlowers = prep2.state.shops.find((s) => s.id === "flowers").unitPrice;
    const perGuest = prep2.visitorsUsed > 0 ? prep2.earnedCoins / prep2.visitorsUsed : null;
    console.log(`    before coins=${coinsBefore} coffee@${priceBefore} → prepare flowers: used=${prep2.visitorsUsed} earned=${prep2.earnedCoins}`);
    console.log(`    after  coins=${prep2.state.coins} coffee@${priceAfter} flowers@${priceAfterFlowers}`);
    if (prep2.visitorsUsed > 0) {
        ok("4-a 本轮客人实付单价 == 响应内该店 unitPrice（表现层飘字与入账一致）",
            perGuest === priceAfter,
            `实付 ${perGuest}/位，但响应里 coffee.unitPrice=${priceAfter} → 飘字会显示 +${priceAfter} 而入账 +${perGuest}`);
    } else {
        ok("4-a 现场未成立（本轮 0 客人），需加长等待重跑", false, "visitorsUsed=0");
    }
    ok("4-b 单价确因满铺加成而跳变（同一响应内）", priceAfter !== priceBefore,
        `${priceBefore} → ${priceAfter}（一层两铺齐了，+${prep2.state.shops.find((s) => s.id === "coffee").unitPrice - priceBefore}）`);

    // ---------- 5. 连点开店 / 并发 ----------
    console.log("\n[5] 连点开店（同一店重复提交）与并发");
    const revBefore = (await call("GET", "/api/v1/mall")).revision;
    const before = (await call("GET", "/api/v1/mall")).coins;
    const clicks = [];
    for (let i = 0; i < 5; i += 1) clicks.push(call("POST", "/api/v1/shops/coffee/prepare"));
    const settled = await Promise.all(clicks);
    const coinsAfter = (await call("GET", "/api/v1/mall")).coins;
    const revAfter = (await call("GET", "/api/v1/mall")).revision;
    ok("5-a 并发重复开店全部 changed=false", settled.every((r) => r.changed === false),
        settled.map((r) => r.changed).join(","));
    ok("5-b 并发重复开店不重复记账、revision 不动", coinsAfter === before && revAfter === revBefore,
        `coins ${before}→${coinsAfter} rev ${revBefore}→${revAfter}`);

    // ---------- 6. 验收 2：完全退出再进入 ----------
    console.log("\n[6] 退出前最后一次权威结算");
    await sleep(5200);
    const last = await call("POST", "/api/v1/mall/settle");
    const committed = last.state;
    console.log("[6] 退出前", JSON.stringify(digest(committed)));
    writeFileSync(OUT, JSON.stringify({ base: BASE, transcript, committed }, null, 2), "utf8");
    console.log(`[6] 交易记录已落 ${OUT}（${transcript.length} 次请求）`);
    // 重进部分由外层脚本负责（要重启进程），结果写在 review-m05-restart.json 里比对。
}

main().catch((err) => {
    console.error("探针自身异常：", err);
    try { writeFileSync(OUT, JSON.stringify({ transcript, results }, null, 2), "utf8"); } catch { /* ignore */ }
    process.exitCode = 1;
});
