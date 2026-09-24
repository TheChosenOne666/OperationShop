// 稳定错误码 → 玩家可读提示。M06 验收第 8 条要求「错误码逐个映射」，本文件就是那份映射。
//
// 为什么不写在 MallScene / MallPopups 里：那两个文件 import "cc"，Node 的类型擦除跑不动，
// 探针就断言不了「每个码都有提示、提示互不相同、没有码落到兜底文案」。刻意做成无依赖的
// 纯映射，`tools/m05_probe.ts` 可以直接把它逐条钉死（项目里没有前端测试框架，这条路径
// 与 MallStore 同一个先例）。
//
// 口径：
//   · 只按**稳定错误码**分支，绝不匹配服务端返回的中文 message（`internal/api/handler.go`
//     的 writeError 文案改了不能把界面带歪，ADR 0001 同口径）。
//   · 提示要说清「为什么不行 + 下一步做什么」，不许出现红色/失败措辞（美术圣经 §9.2 无失败态）。
//   · 未预期的码保留原始码：宁可让玩家看到一个陌生英文码，也不要把服务端的新码显示成
//     一句假装解释过的空话。

/** 服务端与传输层的稳定错误码。键与 `internal/api/handler.go`、`ApiClient.ts` 逐字对齐。 */
export const ERROR_TEXT: Readonly<Record<string, string>> = {
    // ---- 业务拒绝（409 一族，M06 验收第 8 条点名的六个）----
    INSUFFICIENT_COINS: "金币不够，先去经营页多招待几位客人",
    MAX_LEVEL: "这家店已经是最高等级了",
    SLOT_LOCKED: "这个铺位还没解锁",
    SLOT_ORDER: "要按顺序解锁，先解锁前一个铺位",
    SHOP_NOT_OPEN: "这家店还没开业，先在经营页点「开业」",
    SHOP_NOT_FOUND: "找不到这家店铺",
    // ---- 同族但界面走不到的码（点了按钮仍可能被拒，兜住比留空好）----
    CLOCK_BACKWARDS: "服务端时间回退，暂时停止结算",
    NUMERIC_LIMIT: "金币已达数值上限，不再入账",
    SAVE_FAILED: "服务端没存下这一步，本次操作未生效",
    // ---- 会话与传输 ----
    UNAUTHORIZED: "本地会话失效，请重新进入游戏",
    ORIGIN_DENIED: "浏览器来源未获允许",
    LOCAL_ONLY: "这个服务只能在开发机上访问",
    NETWORK: "连接中断，稍后会自动重试",
    TIMEOUT: "连接超时，稍后会自动重试",
    BAD_RESPONSE_JSON: "服务端响应无法解析",
    BUNDLE_LOAD_FAILED: "素材包加载失败，请重新进入",
    UNKNOWN: "操作未生效，请稍后再试",
};

/**
 * 取玩家可读提示。传输层的 `HTTP_4xx` / `HTTP_5xx` 没有逐个列出的价值（状态码由服务端
 * 自由增减），统一按「连接中断」这一类给提示，但**保留原始码**用于取证与排查。
 */
export function errorText(code: string): string {
    const mapped = ERROR_TEXT[code];
    if (mapped) return mapped;
    if (code === "NETWORK" || code === "TIMEOUT" || code.indexOf("HTTP_") === 0) {
        return "连接中断，稍后会自动重试";
    }
    return `操作未生效 ${code}`;
}
