// Boot 场景的启动逻辑：拉一次配置与快照，把服务端给的数字显示出来。
//
// 刻意不含的东西（ADR 0001 服务端权威 / 客户端零计算）：没有单价表、没有客流公式、
// 没有本地虚拟时间。界面显示的每一个数都直接来自最近一次服务端响应。
//
// 每 5 秒轮询 settle 属于 M05（经营闭环），这里不做；本文件只负责"开机取一次数，然后进主界面"。
import { _decorator, Component, director, Label } from "cc";
import { ApiError, fetchConfig, fetchMall } from "./ApiClient";
import type { Config, MallView } from "./ApiTypes";

const { ccclass, property } = _decorator;

@ccclass("Boot")
export class Boot extends Component {
    /** 显示金币与客流的文本节点，在编辑器里拖进来。 */
    @property(Label)
    statusLabel: Label | null = null;

    private config: Config | null = null;

    async start(): Promise<void> {
        this.write("正在连接本地后端…");
        try {
            // 并发取两份：config 决定"上限该不该显示"，mall 决定"现在是多少"。
            const [cfg, mall] = await Promise.all([fetchConfig(), fetchMall()]);
            this.config = cfg;
            this.render(mall);
            console.log(`[m04] 已取到服务端快照 revision=${mall.revision} 规则版本=${cfg.rulesVersion}`);
            // B3.3：Boot 的职责就是"拉到数再进 Mall"。拉不到时停在 Boot 显示错误码，
            // 不进一个只会重复报错的主界面。
            // 必须延一帧：在 start() 里同步 loadScene 会在当前场景仍在激活流程时就被拆掉，
            // 实测引擎报 Error 5000（重复销毁对象）。顺带让 Boot 这屏有个最短可见时间。
            this.scheduleOnce(() => {
                director.loadScene("Mall", (err) => {
                    if (err) console.error("[m04] 进入 Mall 场景失败", err);
                });
            }, 0.2);
        } catch (err) {
            this.showError(err);
        }
    }

    /**
     * 客流按需求文档 §6.1 的口径显示为「已到店 / 上限」。
     * dailyVisitorCap 为 null 表示当天还没冻结、上限尚不适用——这时整槽显示 `— / —`，
     * 不显示分子（显示 0 会被读成"今天还没客人"；主理人 2026-09-21 裁定，见架构现状 §7.2 冲突 L）。
     * 与 MallScene.renderHud 同口径。
     */
    private render(mall: MallView): void {
        const cap = mall.dailyVisitorCap === null ? "—" : String(mall.dailyVisitorCap);
        const served = mall.dailyVisitorCap === null ? "—" : String(mall.visitorsServed);
        this.write(`金币 ${mall.coins}　客流 ${served}/${cap}`);
    }

    private showError(err: unknown): void {
        const code = err instanceof ApiError ? err.code : "UNKNOWN";
        // 只报稳定错误码，不把 message 摊到界面上——那是给人看调试用的。
        this.write(`读取失败：${code}`);
        console.error("[m04] 启动取数失败", err);
    }

    private write(text: string): void {
        if (this.statusLabel) this.statusLabel.string = text;
        console.log(`[m04] ${text}`);
    }
}
