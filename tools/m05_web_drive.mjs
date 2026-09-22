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
//   visibility:<hidden|visible> 改 document.hidden 并派发 visibilitychange，
//                               驱动引擎的 Game.EVENT_HIDE/EVENT_SHOW。
//                               ⚠️ 这是**合成触发**，不等于真后台（rAF 没被浏览器掐断），
//                               真机/开发者工具复验才算结案
//   hud                         读**运行中**界面的 Label 字符串（金币/客流/招牌/在场人数/飘字文本）。
//                               截图只能看"画面变了没"，看不出"显示的是哪个数"——
//                               要归因到 HUD 或飘字金额，只有这条能给可核对的读数
//                               （独立复核 SC-M05-QA-002 P2-E 指的就是这个缺口）。
//                               全部读数收尾时落盘到 <outDir>/hud-reads.json
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
/** 设计坐标系（Canvas 中心系），与引擎的设计分辨率一致，用于换算 dx/dy。 */
const DESIGN = { width: 750, height: 1334 };
/**
 * 仿真视口。**必须不小于产物画布的 CSS 尺寸**：`build/web-desktop/index.html` 把
 * `GameDiv`/`GameCanvas` 写死成 1280×960，`Emulation.setDeviceMetricsOverride` 并不会让它跟着视口缩。
 * 原先这里给的是 750×1334（想保竖屏比例），结果设计坐标 x>146 的铺位换算出的 CSS 坐标
 * 落在视口之外，浏览器直接把触摸事件丢掉——客户端一行日志都不打，看着就是"点了没反应"
 * 的**静默假阴性**（独立复核 SC-M05-QA-002 P1-B）。竖屏构图改用 `clip:` 裁截图保证。
 */
const VIEWPORT = { width: 1280, height: 1400 };

/**
 * 在页面里读**运行时**的 Label 字符串。走 `window.cc`（web-desktop 产物里挂着引擎全局），
 * 按节点路径取，不猜坐标也不认截图观感。`CoinFloat` 是飘字根节点，文本在它的 `Text` 子节点上。
 */
const HUD_READ = `(() => {
    const C = window.cc;
    if (!C) return null;
    const canvas = C.director.getScene().getChildByName('Canvas');
    const text = (path) => {
        const node = canvas.getChildByPath(path);
        const label = node && node.getComponent(C.Label);
        return label ? label.string : null;
    };
    const layer = canvas.getChildByName('GuestLayer');
    const kids = layer ? layer.children : [];

    // ---------- M06 页面层读数 ----------
    // 只在页面可见时读：隐藏节点上的字符串是上一轮的残值，当成"显示值"会读出假证据。
    const pageOf = (name) => {
        const node = canvas.getChildByName(name);
        return node && node.active ? node : null;
    };
    const inPage = (root) => (path) => {
        if (!root) return null;
        const node = root.getChildByPath(path);
        const label = node && node.getComponent(C.Label);
        return label ? label.string : null;
    };
    const activeOf = (root, path) => Boolean(root && root.getChildByPath(path) && root.getChildByPath(path).active);
    const manage = pageOf('PageManage');
    const layout = pageOf('PageLayout');
    const readManage = () => {
        const at = inPage(manage);
        const rows = manage ? manage.getChildByName('List').children.map((row, index) => ({
            i: index,
            // 行在 List 容器下，路径要带上 List/ 前缀（早先漏了这层，整列读成 null）
            name: at(\`List/\${row.name}/Name\`),
            main: at(\`List/\${row.name}/Main\`),
            sub: at(\`List/\${row.name}/Sub\`),
            state: ['Opened', 'OpenButton', 'Locked'].filter((k) => activeOf(manage, \`List/\${row.name}/\${k}\`))[0] || 'none',
            level: at(\`List/\${row.name}/LevelBadge/Level\`),
            badgeOn: activeOf(manage, \`List/\${row.name}/LevelBadge\`),
        })) : null;
        return manage ? {
            visitors: at('Today/Visitors'),
            earned: at('Today/Earned'),
            bonus: at('Today/Bonus'),
            notice: activeOf(manage, 'Notice') ? text('PageManage/Notice') : '',
            rows,
        } : null;
    };
    const readLayout = () => {
        if (!layout) return null;
        const at = inPage(layout);
        return {
            head2: at('Head2/Count'),
            head1: at('Head1/Count'),
            boost: layout.getChildByName('Boost').children.filter((n) => n.name.startsWith('Line')).map((n) => n.getComponent(C.Label).string),
            action: at('Action/Text'),
            actionCost: at('Action/Cost'),
            actionDim: (() => {
                const a = layout.getChildByName('Action');
                const o = a && a.getComponent(C.UIOpacity);
                return o ? o.opacity : 255;
            })(),
            notice: activeOf(layout, 'Notice') ? text('PageLayout/Notice') : '',
            // 铺位卡挂在各楼层的 Slots<层> 容器下，卡名是 Card-<铺位id>
            cardText: layout.children
                .filter((n) => n.name.startsWith('Slots'))
                .flatMap((row) => row.children.map((card) => ({
                    id: card.name.replace('Card-', ''),
                    name: card.getChildByName('Name').getComponent(C.Label).string,
                    status: card.getChildByName('Status').getComponent(C.Label).string,
                }))),
        };
    };

    // 可点节点在 Canvas 中心系下的坐标：click 步骤要的就是这个坐标系，
    // 从运行时量出来比我按稿面推算一遍可靠（节点位置由场景/代码设定，可能与我读稿的理解不符）。
    const center = canvas.getWorldPosition();
    const anchor = (path) => {
        const node = canvas.getChildByPath(path);
        if (!node) return null;
        const p = node.getWorldPosition();
        return [Math.round(p.x - center.x), Math.round(p.y - center.y)];
    };
    const clickable = {};
    for (const path of ['Hud/NavManage', 'Hud/NavLayout', 'Hud/NavFriend']) clickable[path] = anchor(path);
    for (const [page, rows] of [['PageManage', 5], ['PageLayout', 0]]) {
        for (let i = 0; i < rows; i += 1) clickable[\`\${page}/List/Row\${i}/OpenButton\`] = anchor(\`\${page}/List/Row\${i}/OpenButton\`);
        if (page === 'PageLayout') clickable['PageLayout/Action'] = anchor('PageLayout/Action');
    }
    return JSON.stringify({
        coin: text('Hud/CoinLabel'),
        visitor: text('Hud/VisitorLabel'),
        marquee: text('Marquee'),
        guests: kids.filter((c) => c.name === 'Guest').length,
        floats: kids.filter((c) => c.name === 'CoinFloat')
            .map((c) => { const t = c.getChildByName('Text'); return t && t.getComponent(C.Label) ? t.getComponent(C.Label).string : '?'; }),
        page: manage ? 'manage' : layout ? 'layout' : 'home',
        manage: readManage(),
        layout: readLayout(),
        clickable,
    });
})()`;

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
        // 每次取证都关缓存：产物是本地反复重建的，Chrome 复用磁盘缓存会让我们读到**上一版**
        // 引擎或脚本，表现成"改了没生效"的假阴性（2026-09-22 实测踩过：开了 graphics 裁剪后
        // 首跑仍报旧错，字节偏移一模一样，就是缓存里的旧 cc.js）。
        await cdp.send("Network.setCacheDisabled", { cacheDisabled: true });
        // 视口见 VIEWPORT 的注释：保 1:1 不缩，否则触点会被视口裁掉
        await cdp.send("Emulation.setDeviceMetricsOverride", {
            width: VIEWPORT.width, height: VIEWPORT.height, deviceScaleFactor: 2, mobile: true,
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

        /**
         * 点击有没有真的送到引擎。`MallScene.onSlotTouched` 的两条分支必打日志
         * （「铺位 …」或「玩家请求开店：…」），所以等到一条即成立。
         * 没这道检查的话，触点掉出视口 = 静默假阴性，会被读成"功能没反应"（SC-M05-QA-002 P1-B）。
         */
        async function waitTouchDelivered(mark, timeoutMs = 1500) {
            const deadline = Date.now() + timeoutMs;
            while (Date.now() < deadline) {
                for (let i = mark; i < consoleLines.length; i += 1) {
                    if (/\[m0[56]\] (铺位|玩家请求|切页|主按钮|当前页|点了)/.test(consoleLines[i])) return true;
                }
                await sleep(100);
            }
            return false;
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

        /** 未送达引擎的点击（触点被视口裁掉 / 坐标算错），收尾统一报出并以非零码退出。 */
        const silentClicks = [];

        /** 每次 `hud` 步骤的读数，收尾落盘，便于逐条核对「显示值 vs 服务端权威值」。 */
        const hudReads = [];

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
                // ⚠️ 只能定位"该看哪几帧"，**不能当判据**：客人走位、飘字淡出同样会改字节数，
                // 归不到 HUD 数字上（独立复核 SC-M05-QA-002 P2-E）。要读显示值就 clip: 到 HUD 再看图，
                // 或直接读 drive-console.log 里「应用 … 金币=… 已到店=…」那行权威值。
                const changed = sizes.map((s, i) => (i && s !== sizes[i - 1] ? i : -1)).filter((i) => i > 0);
                console.log(`     画面有变化的帧号（不可归因到 HUD）：${changed.length ? changed.join(",") : "无（这段时间画面没变）"}`);
            } else if (kind === "clip") {
                clipDesign = rest.map(Number);
                console.log(`  ✂️ 后续截图裁剪到设计区域 中心(${clipDesign[0]},${clipDesign[1]}) ${clipDesign[2]}×${clipDesign[3]}`);
            } else if (kind === "visibility") {
                const state = rest.join(":");
                const hidden = state === "hidden";
                if (state !== "hidden" && state !== "visible") throw new Error(`visibility 只接受 hidden/visible，收到 ${state}`);
                await evaluate(`(() => {
                    Object.defineProperty(document, 'hidden', { configurable: true, get: () => ${hidden} });
                    Object.defineProperty(document, 'visibilityState', {
                        configurable: true, get: () => ${hidden ? "'hidden'" : "'visible'"},
                    });
                    document.dispatchEvent(new Event('visibilitychange'));
                    return 1;
                })()`);
                console.log(`  🌓 合成 visibilitychange → ${state}（引擎会 emit EVENT_${hidden ? "HIDE" : "SHOW"}）`);
            } else if (kind === "hud") {
                const raw = await evaluate(HUD_READ);
                if (!raw) {
                    // 产物里没有 window.cc（2026-09-22 实测：web-desktop 全量 grep 零命中，
                    // 而 debug=true 会让编辑器去重编引擎并被 SIGTERM 杀掉）。
                    // 改从界面自己打的渲染摘要取证——日志里那行就是各 Label 当时的字符串，
                    // 归因强度不低于从外部读引擎全局，且不需要给产物开调试口子。
                    const applied = consoleLines.filter((l) => l.includes("[m05] 应用")).pop();
                    const render = consoleLines.filter((l) => l.includes("[m06] 渲染")).pop();
                    if (!applied && !render) {
                        // 两条都没有就是真读不到，不能当证据——非零退出，别静默过去
                        console.log("  📊 HUD 读数：既无 window.cc，控制台里也没有 [m05] 应用 / [m06] 渲染 行");
                        process.exitCode = 1;
                    } else {
                        const strip = (line) => (line ? line.replace(/^.*?: /, "") : "无");
                        hudReads.push({ from: "console", server: strip(applied), ui: strip(render) });
                        console.log(`  📊 服务端侧：${strip(applied)}`);
                        console.log(`  📊 界面侧：${strip(render)}`);
                    }
                } else {
                    const h = JSON.parse(raw);
                    hudReads.push(h);
                    console.log(`  📊 金币=${h.coin} 客流=${h.visitor} 招牌=${h.marquee} 在场=${h.guests} 人 飘字=${JSON.stringify(h.floats)}`);
                    if (h.manage) console.log(`  📊 经营页=${JSON.stringify(h.manage)}`);
                    if (h.layout) console.log(`  📊 布局页=${JSON.stringify(h.layout)}`);
                    if (h.clickable) console.log(`  📊 可点坐标=${JSON.stringify(h.clickable)}`);
                }
            } else if (kind === "wait") {
                const secs = Number(rest.join(":"));
                console.log(`  ⏳ 等待 ${secs}s`);
                await sleep(secs * 1000);
            } else if (kind === "click") {
                const [label, dx, dy] = rest;
                const mark = consoleLines.length;
                const at = await clickDesign(Number(dx), Number(dy));
                console.log(`  👆 点击 ${label} 设计(${dx},${dy}) → CSS(${at.x.toFixed(0)},${at.y.toFixed(0)})`);
                if (await waitTouchDelivered(mark)) {
                    console.log("     ✔ 引擎已收到（控制台出现了 [m05] 点击日志）");
                } else {
                    silentClicks.push(`${label} 设计(${dx},${dy}) → CSS(${at.x.toFixed(0)},${at.y.toFixed(0)})`);
                    console.log("     ❌ 1.5s 内没有任何 [m05] 点击日志：事件没送到引擎。这是驱动器的问题，不是功能的问题");
                }
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
        writeFileSync(`${outDir}/hud-reads.json`, JSON.stringify(hudReads, null, 2) + "\n", "utf8");
        console.log(`[m05-drive] 控制台 ${consoleLines.length} 行、异常 ${exceptions.length} 条`);
        if (exceptions.length) {
            console.log("[m05-drive] ⚠️ 有未捕获异常：");
            for (const e of exceptions) console.log(`  - ${e}`);
            process.exitCode = 1;
        }
        if (silentClicks.length) {
            console.log(`[m05-drive] ⚠️ ${silentClicks.length} 次点击未送达引擎——假阴性，别据此判「点了没反应」：`);
            for (const s of silentClicks) console.log(`  - ${s}`);
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
