// 本地开发后端的 HTTP 客户端：只负责「带上令牌发请求、把 JSON 交出去、把错误码原样抛出」。
// 任何数值推算都不在这里（ADR 0001）。
//
// 传输层用 XMLHttpRequest 而不是 wx.request：构建产物里的 engine-adapter.js / web-adapter.js
// 会为小游戏平台提供 XHR 适配，这样同一份代码在浏览器预览和小游戏里都能跑。
// ⚠️ 这是**待实测确认**的假设，不是查证过的事实——首次联调若报"网络错误"，先怀疑这里。
//
// M07 会话（两种构建模式，见 tools/m04_build.py 生成 BuildConfig.ts 时的 MALL_AUTH_MODE）：
//   · dev     —— 用构建期注入的开发令牌直通，ensureSession() 是空操作；
//   · wechat  —— wx.login 拿 code 换服务端会话令牌；请求带票，收到 SESSION_EXPIRED
//                自动重登并重放一次（PRD R3）。web-desktop 产物没有 wx，该模式只能在
//                微信开发者工具 / 真机里跑。
import { BUILD_CONFIG } from "./BuildConfig";
import type { Config, MallView, Result, SessionResponse } from "./ApiTypes";

/** 服务端返回的稳定错误码（internal/api/handler.go）。客户端可据此分支，不要匹配中文提示。 */
export class ApiError extends Error {
    constructor(readonly code: string, readonly status: number) {
        super(`${code} (HTTP ${status})`);
        this.name = "ApiError";
    }
}

/** 微信小游戏的登录接口（运行时由平台提供；web-desktop 产物里没有它）。 */
declare const wx:
    | {
          login(options: {
              success: (res: { code: string }) => void;
              fail: (err: unknown) => void;
          }): void;
      }
    | undefined;

/** 当前会话令牌。dev 模式恒等于开发令牌；wechat 模式登录后由服务端签发。 */
let sessionToken: string | null = null;
/** 进行中的登录：并发调用共享同一次登录，避免"登录风暴"（轮询与点击同时触发重登时）。 */
let loggingIn: Promise<void> | null = null;

/**
 * 确保已登录，之后每个请求都带令牌。
 * dev 模式把开发令牌装好即返回；wechat 模式走一次微信登录换会话令牌。
 * 重复调用是幂等的：已有令牌直接返回，没有令牌时并发调用共享同一次登录。
 */
export function ensureSession(): Promise<void> {
    if (BUILD_CONFIG.authMode !== "wechat") {
        sessionToken = BUILD_CONFIG.devToken;
        return Promise.resolve();
    }
    if (sessionToken) return Promise.resolve();
    if (!loggingIn) {
        loggingIn = wechatLogin().finally(() => {
            loggingIn = null;
        });
    }
    return loggingIn;
}

/** wechat 模式的一次登录：wx.login 拿 code → 换会话令牌。失败抛 ApiError（稳定码）。 */
async function wechatLogin(): Promise<void> {
    if (typeof wx === "undefined") {
        throw new ApiError("WECHAT_LOGIN_FAILED", 0);
    }
    const code = await new Promise<string>((resolve, reject) => {
        wx.login({
            success: (res) => resolve(res.code),
            fail: () => reject(new ApiError("WECHAT_LOGIN_FAILED", 0)),
        });
    });
    const session = await request<SessionResponse>("POST", "/api/v1/session/wechat", JSON.stringify({ code }), false);
    sessionToken = session.token;
    console.log(`[m07] 微信登录成功，会话有效期 ${session.expiresInSeconds} 秒`);
}

/** 从错误响应里取稳定错误码；响应不是 JSON 时保留 HTTP_xxx。 */
function errorCodeOf(xhr: XMLHttpRequest): string {
    try {
        const body = JSON.parse(xhr.responseText);
        const inner = body && body.error;
        if (inner && typeof inner.code === "string") return inner.code;
    } catch {
        /* 响应不是 JSON：保留 HTTP_xxx */
    }
    return `HTTP_${xhr.status}`;
}

function request<T>(method: string, path: string, body = "{}", allowRetry = true): Promise<T> {
    return new Promise<T>((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open(method, BUILD_CONFIG.baseUrl + path, true);
        xhr.setRequestHeader("Content-Type", "application/json");
        xhr.setRequestHeader("Authorization", `Bearer ${sessionToken ?? BUILD_CONFIG.devToken}`);
        xhr.timeout = 8000;
        xhr.onload = () => {
            // 会话过期：自动重登并把本请求重放一次（PRD R3）。只重放一次，重登再失败就把错误交出去。
            if (xhr.status === 401 && allowRetry && errorCodeOf(xhr) === "SESSION_EXPIRED") {
                sessionToken = null;
                ensureSession()
                    .then(() => resolve(request<T>(method, path, body, false)))
                    .catch(reject);
                return;
            }
            if (xhr.status < 200 || xhr.status >= 300) {
                // 服务端错误体形如 {"error":{"code":"SHOP_NOT_FOUND","message":"店铺不存在"}}
                // （internal/api/handler.go 的 writeError）。取 code 分支，message 只给人看。
                reject(new ApiError(errorCodeOf(xhr), xhr.status));
                return;
            }
            try {
                resolve(JSON.parse(xhr.responseText) as T);
            } catch {
                reject(new ApiError("BAD_RESPONSE_JSON", xhr.status));
            }
        };
        xhr.onerror = () => reject(new ApiError("NETWORK", 0));
        xhr.ontimeout = () => reject(new ApiError("TIMEOUT", 0));
        // 写命令按契约只接受空对象 {}；GET 带不带无害，统一发 {} 少一个分支。
        xhr.send(body);
    });
}

/** 规则与数值配置。启动时取一次即可（改了它服务端会拒绝旧存档，见 ADR 0004）。 */
export const fetchConfig = () => request<Config>("GET", "/api/v1/config");

/** 商场全量快照。只在启动首帧拉一次（B3.6「打开游戏：拉一次」）；回前台走 `settle`，因为快照不结算收益。 */
export const fetchMall = () => request<MallView>("GET", "/api/v1/mall");

/**
 * 推进结算。服务端按自己的时钟算，返回体自带最新快照与本次收益。
 * 空闲时 `changed` 为 false 且服务端不落盘——客户端不要据此判断"服务端是否活着"。
 */
export const settle = () => request<Result>("POST", "/api/v1/mall/settle");

/**
 * 开店（把铺位从「待开业」变成「营业中」）。不花金币、幂等：
 * 重复开店返回 `changed=false`，不重复扣费也不写存档（《系统-经营与成长》§2.4）。
 * 铺位未解锁时服务端返回 409 `SLOT_LOCKED`。
 * ⚠️ `shopId` 是店铺 id（`coffee`），不是铺位 id（`f1-s1`）——两套 id 传错一律 404。
 */
export const prepareShop = (shopId: string) =>
    request<Result>("POST", `/api/v1/shops/${encodeURIComponent(shopId)}/prepare`);

/**
 * 升级店铺（1→5 级）。花 `upgradeCost` 金币，下一级起按新单价记账，历史收益不追溯。
 * 未开业返回 `SHOP_NOT_OPEN`、满级返回 `MAX_LEVEL`、余额不够返回 `INSUFFICIENT_COINS`，
 * 三种拒绝都不改变服务端状态。
 * ⚠️ `shopId` 是店铺 id（`coffee`），不是铺位 id（`f1-s1`）。
 */
export const upgradeShop = (shopId: string) =>
    request<Result>("POST", `/api/v1/shops/${encodeURIComponent(shopId)}/upgrade`);

/**
 * 解锁铺位（M06 布局页的主操作，游戏里最大额的不可逆支出）。
 * 跳序返回 `SLOT_ORDER`、已解锁的铺位再解锁返回 `SLOT_LOCKED`、余额不够返回 `INSUFFICIENT_COINS`。
 * ⚠️ 这里用**铺位 id**（`f2-s1`），不是店铺 id——两套 id 传错一律 404 `SHOP_NOT_FOUND`。
 */
export const unlockSlot = (slotId: string) =>
    request<Result>("POST", `/api/v1/slots/${encodeURIComponent(slotId)}/unlock`);
