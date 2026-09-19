// 本地开发后端的 HTTP 客户端：只负责「带上令牌发请求、把 JSON 交出去、把错误码原样抛出」。
// 任何数值推算都不在这里（ADR 0001）。
//
// 传输层用 XMLHttpRequest 而不是 wx.request：构建产物里的 engine-adapter.js / web-adapter.js
// 会为小游戏平台提供 XHR 适配，这样同一份代码在浏览器预览和小游戏里都能跑。
// ⚠️ 这是**待实测确认**的假设，不是查证过的事实——首次联调若报"网络错误"，先怀疑这里。
import { BUILD_CONFIG } from "./BuildConfig";
import type { Config, MallView, Result } from "./ApiTypes";

/** 服务端返回的稳定错误码（internal/api/handler.go）。客户端可据此分支，不要匹配中文提示。 */
export class ApiError extends Error {
    constructor(readonly code: string, readonly status: number) {
        super(`${code} (HTTP ${status})`);
        this.name = "ApiError";
    }
}

function request<T>(method: string, path: string): Promise<T> {
    return new Promise<T>((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open(method, BUILD_CONFIG.baseUrl + path, true);
        xhr.setRequestHeader("Content-Type", "application/json");
        xhr.setRequestHeader("Authorization", `Bearer ${BUILD_CONFIG.devToken}`);
        xhr.timeout = 8000;
        xhr.onload = () => {
            if (xhr.status < 200 || xhr.status >= 300) {
                // 服务端错误体形如 {"error":{"code":"SHOP_NOT_FOUND","message":"店铺不存在"}}
                // （internal/api/handler.go 的 writeError）。取 code 分支，message 只给人看。
                let code = `HTTP_${xhr.status}`;
                try {
                    const body = JSON.parse(xhr.responseText);
                    const inner = body && body.error;
                    if (inner && typeof inner.code === "string") code = inner.code;
                } catch {
                    /* 响应不是 JSON：保留 HTTP_xxx */
                }
                reject(new ApiError(code, xhr.status));
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
        xhr.send("{}");
    });
}

/** 规则与数值配置。启动时取一次即可（改了它服务端会拒绝旧存档，见 ADR 0004）。 */
export const fetchConfig = () => request<Config>("GET", "/api/v1/config");

/** 商场全量快照。切回前台时拉一次（B3.6）。 */
export const fetchMall = () => request<MallView>("GET", "/api/v1/mall");

/**
 * 推进结算。服务端按自己的时钟算，返回体自带最新快照与本次收益。
 * 空闲时 `changed` 为 false 且服务端不落盘——客户端不要据此判断"服务端是否活着"。
 */
export const settle = () => request<Result>("POST", "/api/v1/mall/settle");
