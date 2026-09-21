// M05 端到端驱动器：用无头 Chrome + CDP 打开 web-desktop 构建产物，
// 按脚本化步骤点击、截图、收集控制台，产出可复现的验证证据。
//
// 为什么不用现成的 MCP：chrome-devtools MCP 的 take_screenshot 带 filePath 会被
// 「不在 workspace roots 内」挡掉（它的工作目录是用户主目录），browser-use MCP 能写文件
// 但视口被 DevTools 挤窄、且无 resize。直连 CDP 不需要任何新依赖，
// 还能精确控制视口与 deviceScaleFactor。
//
// 用法：
//   node tools/m05_web_drive.mjs <url> <outDir> <step...>
// 步骤（按顺序执行）：
//   shot:<名字>                 全页截图 → <outDir>/<名字>.png
//   click:<标签>:<dx>:<dy>      点击设计坐标（Canvas 中心系，x 右正 / y 上正）
//   probe:<标签>:<dx>:<dy>      同 click，但额外回报浏览器侧收到的事件计数与 canvas 矩形——
//                               点击没反应时用它判断事件有没有送到页面、坐标算错没算错
//   clip:<dx>:<dy>:<w>:<h>      之后所有截图只截这块设计坐标区域（中心 + 宽高）。
//                               整屏一张 1MB 且看不出细节，抓 400ms 补间必须截小块
//   burst:<名字>:<张数>:<间隔毫秒>  连拍，每张报字节数——
//                               数值变了 PNG 字节数就会变，据此定位该看哪几帧，不必逐张开
//   wait:<秒>                   等待
//   reload                      刷新页面（等价于「完全退出小游戏再进入」）
// 例：
//   node tools/m05_web_drive.mjs http://127.0.0.1:8123 .work/qa-m05 \
//        shot:01-initial click:f1-s1:-180.5:-156.5 wait:1 shot:02-opened wait:12 shot:03-polling
//
// 坐标换算依据：设计分辨率 750×1334、policy 3（FIXED_HEIGHT）。
// ⚠️ 关键：**以 canvas 元素中心为原点**，不是左边缘。FIXED_HEIGHT 把高度锁定为 1334、
//    宽度按窗口比例向外扩（横屏窗口时可见设计宽度会超过 750），而 Canvas 节点始终居中在屏幕上，
//    所以 dx 是从中心量的。按左边缘算会把整次点击平移几百像素、落到场景之外（实测踩过）。
//   scale = canvasCSSHeight / 1334
//   cssX  = canvasLeft + canvasWidth/2  + dx * scale
//   cssY  = canvasTop  + canvasHeight/2 − dy * scale
// 点击走 CDP 的触摸事件（并开启触摸仿真），因为游戏监听的是 TOUCH_END。
import { spawn } from "node:child_process";
import { rmSync, writeFileSync, mkdirSync } from "node:fs";
import { resolve } from "node:path";

const CHROME = "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe";
const CDP_PORT = 9223;
const DESIGN = { width: 750, height: 1334 };

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function parseArgs() {
    const [url, outDir, ...steps] = process.argv.slice(2);
    if (!url || !outDir) {
        console.error("用法：node tools/m05_web_drive.mjs <url> <outDir> <step...>");
        process.exit(2);
    }
    return { url, outDir: resolve(outDir), steps };
}

async function waitForDebugger() {
    for (let i = 0; i < 60; i += 1) {
        try {
            const res = await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`);
            const pages = (await res.json()).filter((t) => t.type === "page");
            if (pages.length) return pages[0];
        } catch {
            /* Chrome 还没起来 */
        }
        await sleep(500);
    }
    throw new Error("连不上 Chrome 的调试端口，确认无头进程已启动");
}

/** 极简 CDP 客户端：请求按 id 配对，事件按 method 排队。 */
class Cdp {
    constructor(socket) {
        this.socket = socket;
        this.nextId = 1;
        this.pending = new Map();
        this.events = [];
        this.handlers = new Map();
        socket.addEventListener("message", (msg) => {
            const data = JSON.parse(msg.data);
            if (data.id !== undefined) {
                const entry = this.pending.get(data.id);
                this.pending.delete(data.id);
                if (!entry) return;
                if (data.error) entry.reject(new Error(JSON.stringify(data.error)));
                else entry.resolve(data.result);
                return;
            }
            this.events.push(data);
            const handler = this.handlers.get(data.method);
            if (handler) handler(data.params);
        });
    }

    send(method, params = {}) {
        const id = this.nextId++;
        return new Promise((res, rej) => {
            this.pending.set(id, { resolve: res, reject: rej });
            this.socket.send(JSON.stringify({ id, method, params }));
        });
    }

    on(method, handler) {
        this.handlers.set(method, handler);
    }
}

async function main() {
    const { url, outDir, steps } = parseArgs();
    mkdirSync(outDir, { recursive: true });
    const userData = `${outDir}/chrome-profile`;
    const consoleLines = [];
    const exceptions = [];

    const chrome = spawn(CHROME, [
        "--headless=new",
        `--remote-debugging-port=${CDP_PORT}`,
        `--user-data-dir=${userData}`,
        "--no-first-run",
        "--disable-gpu",
        "--window-size=800,1400",
        "about:blank",
    ], { stdio: "ignore" });

    try {
        const target = await waitForDebugger();
        const socket = new WebSocket(target.webSocketDebuggerUrl);
        await new Promise((res, rej) => {
            socket.addEventListener("open", res, { once: true });
            socket.addEventListener("error", rej, { once: true });
        });
        const cdp = new Cdp(socket);

        await cdp.send("Runtime.enable");
        await cdp.send("Log.enable");
        await cdp.send("Page.enable");
        await cdp.send("Network.enable");
        // 视口给成竖屏 750×1334：与小游戏的真实比例一致，
        // 免得 FIXED_HEIGHT 把横向视野撑宽、截图里两侧全是空地
        await cdp.send("Emulation.setDeviceMetricsOverride", {
            width: DESIGN.width, height: DESIGN.height, deviceScaleFactor: 2, mobile: true,
        });
        await cdp.send("Emulation.setTouchEmulationEnabled", { enabled: true });

        cdp.on("Runtime.consoleAPICalled", (p) => {
            const text = (p.args || []).map((a) => a.value ?? a.description ?? a.unserializableValue ?? "").join(" ");
            consoleLines.push(`${p.type}: ${text}`);
        });
        cdp.on("Runtime.exceptionThrown", (p) => {
            exceptions.push(p.exceptionDetails?.exception?.description ?? p.exceptionDetails?.text ?? "unknown");
        });
        cdp.on("Log.entryAdded", (p) => {
            if (p.entry.level === "error" || p.entry.level === "warning") {
                consoleLines.push(`[${p.entry.source}] ${p.entry.level}: ${p.entry.text}`);
            }
        });

        await cdp.send("Page.navigate", { url });
        await sleep(6000);   // 引擎启动 + Bundle 加载 + 首轮取数

        /** 把设计坐标（Canvas 中心系）换算成页面 CSS 坐标。 */
        async function toCss(dx, dy) {
            const { result } = await cdp.send("Runtime.evaluate", {
                expression: `(() => { const r = document.querySelector('canvas').getBoundingClientRect();
                    return JSON.stringify({ l: r.left, t: r.top, w: r.width, h: r.height }); })()`,
                returnByValue: true,
            });
            const rect = JSON.parse(result.value);
            const scale = rect.h / DESIGN.height;
            return {
                x: rect.l + rect.w / 2 + dx * scale,
                y: rect.t + rect.h / 2 - dy * scale,
            };
        }

        async function clickDesign(dx, dy) {
            const { x, y } = await toCss(dx, dy);
            const point = { x, y };
            await cdp.send("Input.dispatchTouchEvent", {
                type: "touchStart", touchPoints: [{ ...point, id: 1 }],
            });
            await sleep(60);
            await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
            return { x, y };
        }

        async function evaluate(expression) {
            const { result } = await cdp.send("Runtime.evaluate", { expression, returnByValue: true });
            return result.value;
        }

        /** 点击并回报浏览器侧收到的事件计数——用来区分「坐标算错」与「引擎没接触摸」。 */
        async function probeClick(dx, dy) {
            await evaluate(`(() => {
                window.__ev = { touchstart: 0, touchend: 0, pointerdown: 0, mousedown: 0 };
                for (const t of Object.keys(window.__ev)) {
                    document.addEventListener(t, () => { window.__ev[t] += 1; }, true);
                }
                const r = document.querySelector('canvas').getBoundingClientRect();
                window.__rect = { l: r.left, t: r.top, w: r.width, h: r.height };
                return 1;
            })()`);
            const at = await clickDesign(dx, dy);
            const seen = await evaluate("JSON.stringify({ ev: window.__ev, rect: window.__rect })");
            console.log(`  🔎 探针 ${JSON.stringify({ at: { x: Math.round(at.x), y: Math.round(at.y) } })} 事件=${seen}`);
            return at;
        }

        /** 非空时后续截图只截这块设计坐标区域（中心 + 宽高）。 */
        let clipDesign = null;

        async function canvasRect() {
            const value = await evaluate(`(() => { const r = document.querySelector('canvas').getBoundingClientRect();
                return JSON.stringify({ l: r.left, t: r.top, w: r.width, h: r.height }); })()`);
            return JSON.parse(value);
        }

        /** 把设计坐标区域换算成 CDP 的 CSS 像素裁剪框（同样以 canvas 中心为原点）。 */
        async function toClip() {
            const rect = await canvasRect();
            const [dx, dy, w, h] = clipDesign;
            const scale = rect.h / DESIGN.height;
            return {
                x: rect.l + rect.w / 2 + (dx - w / 2) * scale,
                y: rect.t + rect.h / 2 - (dy + h / 2) * scale,
                width: w * scale,
                height: h * scale,
                scale: 1,
            };
        }

        /** 截一张图，返回字节数——连拍时靠它定位"哪一帧数值变了"，不必逐张开图。 */
        async function shot(name) {
            const params = { format: "png" };
            if (clipDesign) params.clip = await toClip();
            else params.captureBeyondViewport = true;
            const { data } = await cdp.send("Page.captureScreenshot", params);
            const file = `${outDir}/${name}.png`;
            const bytes = Buffer.from(data, "base64");
            writeFileSync(file, bytes);
            return { file, size: bytes.length };
        }

        console.log(`[m05-drive] 打开 ${url}`);
        for (const step of steps) {
            const [kind, ...rest] = step.split(":");
            if (kind === "shot") {
                const { file, size } = await shot(rest.join(":"));
                console.log(`  📷 ${file}（${size} 字节）`);
            } else if (kind === "burst") {
                const [prefix, count, intervalMs] = rest;
                const sizes = [];
                for (let i = 0; i < Number(count); i += 1) {
                    const { size } = await shot(`${prefix}-${String(i).padStart(2, "0")}`);
                    sizes.push(size);
                    await sleep(Number(intervalMs));
                }
                console.log(`  🎞 连拍 ${sizes.length} 张：${sizes.join(",")}`);
                // 字节数变化 ≈ 画面内容变了（数字换了字形），指出该看哪几帧
                const changed = sizes.map((s, i) => (i && s !== sizes[i - 1] ? i : -1)).filter((i) => i > 0);
                console.log(`     字节数变化的帧号：${changed.length ? changed.join(",") : "无（这段时间画面没变）"}`);
            } else if (kind === "clip") {
                clipDesign = rest.map(Number);
                console.log(`  ✂️ 后续截图裁剪到设计区域 中心(${clipDesign[0]},${clipDesign[1]}) ${clipDesign[2]}×${clipDesign[3]}`);
            } else if (kind === "wait") {
                const secs = Number(rest.join(":"));
                console.log(`  ⏳ 等待 ${secs}s`);
                await sleep(secs * 1000);
            } else if (kind === "click") {
                const [label, dx, dy] = rest;
                const at = await clickDesign(Number(dx), Number(dy));
                console.log(`  👆 点击 ${label} 设计(${dx},${dy}) → CSS(${at.x.toFixed(0)},${at.y.toFixed(0)})`);
            } else if (kind === "probe") {
                const [label, dx, dy] = rest;
                await probeClick(Number(dx), Number(dy));
            } else if (kind === "reload") {
                console.log("  🔄 刷新页面（等价于完全退出重进）");
                await cdp.send("Page.reload");
                await sleep(6000);
            } else {
                throw new Error(`未知步骤：${step}`);
            }
        }

        // 顺带把控制台与异常落盘，作为可复核的证据
        writeFileSync(`${outDir}/drive-console.log`, consoleLines.join("\n") + "\n", "utf8");
        writeFileSync(`${outDir}/drive-exceptions.log`, exceptions.join("\n") + "\n", "utf8");
        console.log(`[m05-drive] 控制台 ${consoleLines.length} 行、异常 ${exceptions.length} 条`);
        if (exceptions.length) {
            console.log("[m05-drive] ⚠️ 有未捕获异常：");
            for (const e of exceptions) console.log(`  - ${e}`);
            process.exitCode = 1;
        }
    } finally {
        chrome.kill();
        // Chrome 刚退出时目录常被占用，EPERM 属正常，忽略
        try {
            rmSync(userData, { recursive: true, force: true, maxRetries: 3 });
        } catch { /* 忽略 */ }
    }
}

main().catch((err) => {
    console.error("[m05-drive] 失败：", err);
    process.exitCode = 1;
});
