// Mall 场景的主控：把服务端快照映射到 5 个铺位与底部 HUD，美术全部按路径从 Bundle 异步取。
//
// 沿用的硬约束：
//   ADR 0001 服务端权威 / 客户端零计算 —— 只读服务端给的字段，不推算单价、不推算解锁价。
//   架构现状 §10 渲染约定 —— 三态 S1/S2/S3 用「同一张贴图 + 材质自发光档位 + 独立遮挡子节点」表达，
//                                 不预烘多套 PNG；自发光两档（S1 暗 0.30 / S2·S3 亮 1.00）。
//   草案 B3.4 分包策略 —— 铺位立绘与空铺底图在分包 mall_art，HUD 常驻件在主包内 Bundle ui_main。
//   为什么全走路径加载：非 resources/Bundle 目录的资源不支持按路径加载，且手写场景里写 uuid
//   资产引用最容易错；把两批素材都配成 Bundle 后，场景只留结构，取图全在代码里，可复现。
import { _decorator, assetManager, Bundle, Component, Label, Node, Sprite, SpriteFrame } from "cc";
import { ApiError, fetchConfig, fetchMall } from "./ApiClient";
import type { Config, MallView } from "./ApiTypes";

const { ccclass } = _decorator;

/** 分包：铺位立绘、空铺底图、道具、客人。构建后应落在 subpackages/（分包标记待结案）。 */
const ART_BUNDLE = "mall_art";
/** 主包内的 Bundle：背景、HUD 木条、图标、状态遮挡件——首屏必需，随主包启动。 */
const UI_BUNDLE = "ui_main";

/** 架构现状 §10：S1 暗、S2 与 S3 亮，只有两档。 */
const EMISSIVE_DIM = 0.3;
const EMISSIVE_FULL = 1.0;

/** 三态判据（服务端已给够）：unlockedSlots 不含该铺 ⇒ S1；含而 prepared=false ⇒ S2；prepared=true ⇒ S3。 */
type SlotState = "S1" | "S2" | "S3";

/** 与状态无关的常驻件：节点路径 →（所属 Bundle，Bundle 内路径）。一次配齐，免得散在各处。 */
const STATIC_ART: ReadonlyArray<readonly [string, string, string]> = [
    ["Bg", UI_BUNDLE, "bg/bg_mall_base/spriteFrame"],
    ["Floor2/Railing", ART_BUNDLE, "prp/prp_cat_sleep/spriteFrame"],
    ["Hud/Bar", UI_BUNDLE, "ui/ui_hud_bar/spriteFrame"],
    ["Hud/CoinIcon", UI_BUNDLE, "ico/ico_coin_ring/spriteFrame"],
    ["Hud/NavManage/Icon", UI_BUNDLE, "ico/ico_nav_manage/spriteFrame"],
    ["Hud/NavLayout/Icon", UI_BUNDLE, "ico/ico_nav_layout/spriteFrame"],
    ["Hud/NavFriend/Icon", UI_BUNDLE, "ico/ico_nav_friend/spriteFrame"],
    ["Hud/VisitorIcon", UI_BUNDLE, "ico/ico_visitor/spriteFrame"],
];

/** 铺位上恒定的遮挡件（S1 才显示，切换靠 active 不靠换图）。 */
const SLOT_OVERLAY: ReadonlyArray<readonly [string, string]> = [
    ["Veil", "state/ovl_veil_locked/spriteFrame"],
    ["Lock", "state/ico_lock/spriteFrame"],
];

@ccclass("MallScene")
export class MallScene extends Component {
    private bundles = new Map<string, Bundle>();
    private config: Config | null = null;

    async start(): Promise<void> {
        console.log("[m04] Mall 场景启动，准备加载 Bundle", ART_BUNDLE, UI_BUNDLE);
        try {
            const [art, ui, cfg, mall] = await Promise.all([
                this.loadBundle(ART_BUNDLE),
                this.loadBundle(UI_BUNDLE),
                fetchConfig(),
                fetchMall(),
            ]);
            this.bundles.set(ART_BUNDLE, art);
            this.bundles.set(UI_BUNDLE, ui);
            this.config = cfg;
            this.applyStaticArt();
            this.renderSlots(mall);
            this.renderHud(mall);
            console.log(`[m04] Mall 渲染完成 revision=${mall.revision} 铺位=${mall.shops.length}`);
        } catch (err) {
            this.showError(err);
        }
    }

    /** Bundle 加载失败不许白屏：铺位仍要有文字状态可读，所以只记日志、由调用侧决定降级。 */
    private loadBundle(name: string): Promise<Bundle> {
        return new Promise((resolve, reject) => {
            assetManager.loadBundle(name, (err, bundle) => {
                if (err || !bundle) {
                    console.error(`[m04] Bundle ${name} 加载失败`, err);
                    reject(new ApiError("BUNDLE_LOAD_FAILED", 0));
                    return;
                }
                resolve(bundle);
            });
        });
    }

    /** 取 SpriteFrame 并挂到指定路径的 Sprite 上；缺件只报日志，不影响其他节点。 */
    private applyFrame(nodePath: string, bundleName: string, framePath: string): void {
        const sprite = this.node.getChildByPath(nodePath)?.getComponent(Sprite);
        if (!sprite) {
            console.error(`[m04] 场景里找不到带 Sprite 的节点 ${nodePath}`);
            return;
        }
        const bundle = this.bundles.get(bundleName);
        if (!bundle) return;
        bundle.load(framePath, SpriteFrame, (err, frame) => {
            if (err || !frame) {
                // 素材目前是占位图，真素材替换前缺件属预期；这里必须出声，否则以后无从排查。
                console.error(`[m04] ${bundleName} 内取不到 ${framePath}`, err);
                return;
            }
            sprite.spriteFrame = frame;
        });
    }

    private applyStaticArt(): void {
        for (const [nodePath, bundleName, framePath] of STATIC_ART) {
            this.applyFrame(nodePath, bundleName, framePath);
        }
    }

    private renderSlots(mall: MallView): void {
        if (!this.config) return;
        for (const slot of this.config.slots) {
            const found = this.findSlot(slot.id);
            if (!found) {
                console.error(`[m04] 场景里找不到铺位节点 ${slot.id}`);
                continue;
            }
            const { root, path } = found;
            const shop = mall.shops.find((s) => s.id === slot.shopId) ?? null;
            const unlocked = mall.unlockedSlots.indexOf(slot.id) >= 0;
            const state: SlotState = !unlocked ? "S1" : shop && shop.prepared ? "S3" : "S2";

            // S3 换成立绘；S1/S2 都显示该楼层的空铺底图（B3.5，也是 M04 验收第 3 条）。
            const frame = state === "S3"
                ? `shop/shop_${slot.shopId}_open/spriteFrame`
                : `slt/slt_f${slot.floor}_empty/spriteFrame`;
            this.applyFrame(`${path}/Art`, ART_BUNDLE, frame);
            for (const [child, overlayPath] of SLOT_OVERLAY) {
                this.applyFrame(`${path}/${child}`, UI_BUNDLE, overlayPath);
                const overlay = root.getChildByName(child);
                if (overlay) overlay.active = state === "S1";
            }
            this.applyEmissive(root, state === "S1" ? EMISSIVE_DIM : EMISSIVE_FULL);

            const tag = root.getChildByName("Tag")?.getComponent(Label);
            if (tag) {
                if (state === "S3" && shop) tag.string = shop.name;
                else if (state === "S2") tag.string = "待开业";
                else tag.string = mall.nextSlotId === slot.id ? `解锁 $${mall.nextUnlockCost}` : "未开放";
            }
        }
    }

    /** HUD 五槽：金币图标 + 金币数值 + 3 个导航按钮 + 客流。数值全部来自服务端。 */
    private renderHud(mall: MallView): void {
        const coin = this.node.getChildByPath("Hud/CoinLabel")?.getComponent(Label);
        if (coin) coin.string = String(mall.coins);

        const visitor = this.node.getChildByPath("Hud/VisitorLabel")?.getComponent(Label);
        if (visitor) {
            // dailyVisitorCap 为 null 表示当天还没冻结，显示 — 而不是假的 0（与 Boot 同口径）。
            const cap = mall.dailyVisitorCap === null ? "—" : String(mall.dailyVisitorCap);
            visitor.string = `${mall.visitorsServed}/${cap}`;
        }
    }

    /** 铺位挂在两层之一，返回节点与它相对 Canvas 的路径（按路径加载素材要用全路径）。 */
    private findSlot(slotId: string): { root: Node; path: string } | null {
        for (const floor of ["Floor1", "Floor2"]) {
            const path = `${floor}/${slotId}`;
            const root = this.node.getChildByPath(path);
            if (root) return { root, path };
        }
        return null;
    }

    /** 自发光档位靠自定义材质（§10）；材质还没接入时这里是安全的空操作。 */
    private applyEmissive(slotRoot: Node, value: number): void {
        const mat = slotRoot.getChildByName("Art")?.getComponent(Sprite)?.customMaterial;
        if (mat) mat.setProperty("u_emissive", value);
    }

    private showError(err: unknown): void {
        const code = err instanceof ApiError ? err.code : "UNKNOWN";
        const label = this.node.getChildByPath("Hud/CoinLabel")?.getComponent(Label);
        if (label) label.string = `读取失败 ${code}`;
        console.error("[m04] Mall 渲染失败", err);
    }
}
