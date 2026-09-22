// SC-M05-QA-002 复核方自写驱动器（不改实现者留下的 tools/m05_web_drive.mjs）。
//
// 与 m05_web_drive.mjs 的两点不同，都是为了独立取证：
//   1) 视口给成 1280×1400，让整块画布落在视口内。原驱动器把视口仿真成 750×1334，
//      而产物画布是 1280 CSS 宽，于是设计坐标 x>146 的节点（含 f1-s2 与二层全部铺位）
//      换算出的 CSS 点落在视口之外，触摸事件被浏览器丢掉——点击"没反应"不是游戏的错。
//   2) 增加 evalfile 步骤：直接在运行中的场景里读 HUD Label 的**字符串**。
//      「界面显示的是什么」于是有了一手证据，不必靠截图观感或控制台转述来判断。
//
// 用法：node tools/review-m05-drive2.mjs <url> <outDir> <step...>
//   shot:<名字>                     全页截图
//   tap:<dx>:<dy>                   点设计坐标（画布中心系，x 右正 / y 上正）
//   evalfile:<绝对路径.js>          在页面里执行该文件的表达式，打印返回值
//   wait:<秒>  reload  visibility:<hidden|visible>
import { spawn } from "node:child_process";
import { readFileSync, writeFileSync, mkdirSync, rmSync } from "node:fs";
import { resolve } from "node:path";

const CHROME = "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe";
const PORT = 9234;
const DESIGN_H = 1334;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function waitDebugger() {
    return (async () => {
        for (let i = 0; i < 80; i += 1) {
            try {
                const r = await fetch(`http://127.0.0.1:${PORT}/json/list`);
                const pages = (await r.json()).filter((t) => t.type === "page");
                if (pages.length) return pages[0];
            } catch { /* 还没起来 */ }
            await sleep(250);
        }
        throw new Error("连不上调试端口");
    })();
}

class Cdp {
    constructor(socket) {
        this.socket = socket; this.nextId = 1; this.pending = new Map(); this.handlers = new Map();
        socket.addEventListener("message", (m) => {
            const d = JSON.parse(m.data);
            if (d.id !== undefined) {
                const p = this.pending.get(d.id); this.pending.delete(d.id);
                if (!p) return;
                if (d.error) p.reject(new Error(JSON.stringify(d.error))); else p.resolve(d.result);
                return;
            }
            const h = this.handlers.get(d.method); if (h) h(d.params);
        });
    }
    send(method, params = {}) {
        const id = this.nextId++;
        return new Promise((res, rej) => { this.pending.set(id, { resolve: res, reject: rej }); this.socket.send(JSON.stringify({ id, method, params })); });
    }
    on(m, h) { this.handlers.set(m, h); }
}

const [url, outDirRaw, ...steps] = process.argv.slice(2);
if (!url || !outDirRaw) { console.error("缺参数"); process.exit(2); }
const outDir = resolve(outDirRaw);
mkdirSync(outDir, { recursive: true });
const consoleLines = [];
const exceptions = [];

const chrome = spawn(CHROME, [
    "--headless=new", `--remote-debugging-port=${PORT}`, `--user-data-dir=${outDir}/profile`,
    "--no-first-run", "--disable-gpu", "--window-size=1280,1400", "about:blank",
], { stdio: "ignore" });

try {
    const target = await waitDebugger();
    const socket = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { socket.addEventListener("open", res, { once: true }); socket.addEventListener("error", rej, { once: true }); });
    const cdp = new Cdp(socket);
    await cdp.send("Runtime.enable");
    await cdp.send("Log.enable");
    await cdp.send("Page.enable");
    await cdp.send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 1400, deviceScaleFactor: 1, mobile: true });
    await cdp.send("Emulation.setTouchEmulationEnabled", { enabled: true });
    cdp.on("Runtime.consoleAPICalled", (p) => consoleLines.push(`${p.type}: ${(p.args || []).map((a) => a.value ?? a.description ?? "").join(" ")}`));
    cdp.on("Runtime.exceptionThrown", (p) => exceptions.push(p.exceptionDetails?.exception?.description ?? "unknown"));
    cdp.on("Log.entryAdded", (p) => { if (p.entry.level === "error" || p.entry.level === "warning") consoleLines.push(`[${p.entry.source}] ${p.entry.level}: ${p.entry.text}`); });

    await cdp.send("Page.navigate", { url });
    await sleep(7000);

    async function evaluate(expression) {
        const r = await cdp.send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true });
        if (r.exceptionDetails) return `EVAL_ERROR ${r.exceptionDetails.exception?.description ?? r.exceptionDetails.text}`;
        return r.result.value;
    }
    async function canvasRect() {
        return JSON.parse(await evaluate(`JSON.stringify(document.querySelector('canvas').getBoundingClientRect())`));
    }
    async function tap(dx, dy) {
        const r = await canvasRect();
        const scale = r.height / DESIGN_H;
        const x = r.left + r.width / 2 + dx * scale;
        const y = r.top + r.height / 2 - dy * scale;
        if (x < 0 || y < 0 || x > r.left + r.width + 400) { console.log(`  ⚠️ 触点 ${x},${y} 可疑`); }
        await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y, id: 1 }] });
        await sleep(70);
        await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
        console.log(`  👆 tap 设计(${dx},${dy}) → CSS(${x.toFixed(0)},${y.toFixed(0)}) 画布=${r.width.toFixed(0)}x${r.height.toFixed(0)}@(${r.left.toFixed(0)},${r.top.toFixed(0)})`);
    }
    async function shot(name) {
        const { data } = await cdp.send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
        const b = Buffer.from(data, "base64");
        writeFileSync(`${outDir}/${name}.png`, b);
        console.log(`  📷 ${name}.png ${b.length}`);
    }

    for (const step of steps) {
        const [kind, ...rest] = step.split(":");
        if (kind === "shot") await shot(rest.join(":"));
        else if (kind === "tap") await tap(Number(rest[0]), Number(rest[1]));
        else if (kind === "wait") { const s = Number(rest.join(":")); console.log(`  ⏳ ${s}s`); await sleep(s * 1000); }
        else if (kind === "evalfile") {
            const expr = readFileSync(rest.join(":"), "utf8");
            console.log(`  🧪 ${rest.join(":")} → ${await evaluate(expr)}`);
        } else if (kind === "visibility") {
            const hidden = rest.join(":") === "hidden";
            console.log(`  🌓 ${await evaluate(`(() => { Object.defineProperty(document,'hidden',{configurable:true,get:()=>${hidden}}); Object.defineProperty(document,'visibilityState',{configurable:true,get:()=>${hidden ? "'hidden'" : "'visible'"}}); document.dispatchEvent(new Event('visibilitychange')); return 'dispatched'; })()`)}`);
        } else if (kind === "reload") { console.log("  🔄 reload"); await cdp.send("Page.reload"); await sleep(7000); }
        else throw new Error(`未知步骤 ${step}`);
    }
    writeFileSync(`${outDir}/console.log`, consoleLines.join("\n") + "\n", "utf8");
    console.log(`[drive2] 控制台 ${consoleLines.length} 行、未捕获异常 ${exceptions.length} 条`);
    for (const e of exceptions) console.log("  EXC " + e);
    if (exceptions.length) process.exitCode = 1;
} finally {
    chrome.kill();
    try { rmSync(`${outDir}/profile`, { recursive: true, force: true, maxRetries: 3 }); } catch { /* EPERM 忽略 */ }
}
