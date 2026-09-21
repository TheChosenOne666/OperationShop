// Mall 场景的主控：把服务端快照映射到 5 个铺位与底部 HUD，并驱动 M05 的经营闭环。
//
// 沿用的硬约束：
//   ADR 0001 服务端权威 / 客户端零计算 —— 只读服务端给的字段，不推算单价、不推算解锁价。
//       什么时候问服务端、响应信不信、这轮谁来了，全部委托给 MallStore（编排层）。
//   ADR 0004 数值外置 —— 轮询间隔取 config.visitorIntervalSeconds，不在客户端写死秒数。
//   架构现状 §10 渲染约定 —— 三态 S1/S2/S3 用「同一张贴图 + 材质自发光档位 + 独立遮挡子节点」表达，
//                                 不预烘多套 PNG；自发光两档（S1 暗 0.30 / S2·S3 亮 1.00）。
//   草案 B3.4 分包策略 —— 铺位立绘与空铺底图在分包 mall_art，HUD 常驻件在主包内 Bundle ui_main。
//   为什么全走路径加载：非 resources/Bundle 目录的资源不支持按路径加载，且手写场景里写 uuid
//   资产引用最容易错；把两批素材都配成 Bundle 后，场景只留结构，取图全在代码里，可复现。
//
// ⚠️ 渲染必须是**同步且幂等**的：本场景每 5 秒重渲染一次，若渲染里还夹着异步取图，
//    回调会在多轮之间堆积、顺序不可控。所以启动时一次性预加载全部帧，之后渲染只读缓存。
import { _decorator, assetManager, Bundle, Component, EffectAsset, game, Game, Label, Material, Node, Sprite, SpriteFrame } from "cc";
import { ApiError, fetchConfig, fetchMall, prepareShop, settle } from "./ApiClient";
import type { Config, MallView, SlotConfig, ShopView } from "./ApiTypes";
import { MallStore } from "./MallStore";
import type { StoreOp, StoreUpdate } from "./MallStore";
import { GuestStage } from "./GuestStage";
import type { GuestArt } from "./GuestStage";
import { rollNumber, stopRoll } from "./NumberRoll";

const { ccclass } = _decorator;

/** 分包：铺位立绘、空铺底图、道具、客人。构建后应落在 subpackages/（分包标记待结案）。 */
const ART_BUNDLE = "mall_art";
/** 主包内的 Bundle：背景、HUD 木条、图标、状态遮挡件——首屏必需，随主包启动。 */
const UI_BUNDLE = "ui_main";

/** 三态材质用的着色器：放在主包 Bundle 里按路径取，避免在场景里写 uuid 引用。 */
const EFFECT_PATH = "effects/slot-emissive";

/** 架构现状 §10：S1 暗、S2 与 S3 亮，只有两档。 */
const EMISSIVE_DIM = 0.3;
const EMISSIVE_FULL = 1.0;

/** 错误横幅在招牌上停留多久。取 3 秒：短过一次轮询间隔，不会盖住下一次正常反馈。 */
const ERROR_BANNER_SECONDS = 3;

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

/**
 * M05 表演层要用的图：客人三视图在分包 mall_art，飘字金币在主包内 ui_main
 * （`ico_coin` 2026-09-21 从工程外 `assets/_staging/` 挪回来，依据《美术圣经》第 414 行）。
 */
const GUEST_ART: Readonly<Record<keyof GuestArt, readonly [string, string]>> = {
    side: [ART_BUNDLE, "chr/chr_guest_a_side/spriteFrame"],
    front: [ART_BUNDLE, "chr/chr_guest_a_front/spriteFrame"],
    back: [ART_BUNDLE, "chr/chr_guest_a_back/spriteFrame"],
    coin: [UI_BUNDLE, "ico/ico_coin/spriteFrame"],
};

/** 帧缓存的键：同名帧可能分属两个 Bundle，必须带上前缀。 */
function frameKey(bundleName: string, framePath: string): string {
    return `${bundleName}:${framePath}`;
}

@ccclass("MallScene")
export class MallScene extends Component {
    private bundles = new Map<string, Bundle>();
    private frames = new Map<string, SpriteFrame>();
    private config: Config | null = null;
    private effect: EffectAsset | null = null;
    private store: MallStore | null = null;
    private guests: GuestStage | null = null;
    /** 招牌原文。错误横幅要占这块地方，成功后得还原回去。 */
    private marqueeText = "";

    async start(): Promise<void> {
        console.log("[m05] Mall 场景启动，准备加载 Bundle", ART_BUNDLE, UI_BUNDLE);
        try {
            const [art, ui, cfg] = await Promise.all([
                this.loadBundle(ART_BUNDLE),
                this.loadBundle(UI_BUNDLE),
                fetchConfig(),
            ]);
            this.bundles.set(ART_BUNDLE, art);
            this.bundles.set(UI_BUNDLE, ui);
            this.config = cfg;
            this.marqueeText = this.node.getChildByPath("Marquee")?.getComponent(Label)?.string ?? "";
            await Promise.all([this.loadSlotEffect(), this.preloadFrames(cfg)]);
            this.applyStaticArt();
            this.bindSlotInput(cfg);
            this.buildGuestStage(cfg);

            // 传输层直接复用 ApiClient 的三个函数；界面层就是本组件。
            this.store = new MallStore({ fetchMall, settle, prepareShop }, {
                onStoreUpdate: (update) => this.onStoreUpdate(update),
                onStoreError: (code, op) => this.onStoreError(code, op),
            });
            await this.store.load();
            this.startPolling(cfg.visitorIntervalSeconds);
            this.bindLifecycle();
        } catch (err) {
            this.onFatal(err);
        }
    }

    /**
     * 只清引擎**不会**替我清的东西：
     *   · `game.on` 的监听挂在 game 这个 EventTarget 上，与节点生命周期无关；
     *   · 补间的目标是 Node / UIOpacity / 中间态普通对象，走的是 ActionManager，不是本组件的调度器。
     * 而 `schedule` / `scheduleOnce` 的回调不必在这里 unschedule——引擎在 `_onPreDestroy()`
     * 里、`onDestroy` 之前就已经 `unscheduleAllCallbacks()`（`scene-graph/component.ts:407`）。
     */
    protected onDestroy(): void {
        game.off(Game.EVENT_HIDE, this.onGameHide, this);
        game.off(Game.EVENT_SHOW, this.onGameShow, this);
        this.guests?.dispose();
        for (const path of ["Hud/CoinLabel", "Hud/VisitorLabel"]) {
            const label = this.node.getChildByPath(path)?.getComponent(Label);
            if (label) stopRoll(label);
        }
    }

    // ---------- 服务端状态 ----------

    /**
     * 一次成功的响应：重渲染 + 清掉错误横幅。
     * 在飞请求可能在场景销毁后才回来，所以先判 isValid，不去碰已销毁的节点。
     */
    private onStoreUpdate(update: StoreUpdate): void {
        if (!this.isValid) return;
        this.render(update.view);
        this.clearError();
        this.guests?.spawn(update);
        // 注意口径：MallStore 那条日志的「本轮到店」是**人数**，这里的是**几家店**，别说成同一个量。
        console.log(`[m05] 界面已更新 revision=${update.view.revision} 来源=${update.op} 到店涉及 ${update.arrivals.length} 家`);
    }

    /**
     * 一次失败的响应：**只挂横幅，不动数值**——金币与客流保留最后一次成功值。
     * 轮询模式下失败是常态化的瞬时事件，盖掉数字本身就会被看成"金币跳变"（验收第 3 条）。
     */
    private onStoreError(code: string, op: StoreOp): void {
        if (!this.isValid) return;
        const marquee = this.node.getChildByPath("Marquee")?.getComponent(Label);
        if (!marquee) return;
        // 只按稳定错误码分支，不匹配服务端的中文提示（ADR 0001 同口径）。
        const transport = code === "NETWORK" || code === "TIMEOUT" || code.indexOf("HTTP_") === 0;
        marquee.string = `${transport ? "连接中断" : "操作失败"} ${code}`;
        // 定时自清，不能只等"下一次成功"来清：见 clearError 的注释
        this.unschedule(this.clearError);
        this.scheduleOnce(this.clearError, ERROR_BANNER_SECONDS);
        console.error(`[m05] ${op} 失败（${code}），横幅挂 ${ERROR_BANNER_SECONDS} 秒，数值保留最后一次成功值`);
    }

    /**
     * 还原招牌。
     * ⚠️ 必须能**定时自清**：空闲时的 settle 响应会被 revision 守卫丢弃、根本不触发 onStoreUpdate，
     *    只靠"下一次成功更新"来清的话，当日客流耗尽后错误就会永久卡在招牌上（实测踩过）。
     */
    private clearError = (): void => {
        if (!this.isValid) return;
        const marquee = this.node.getChildByPath("Marquee")?.getComponent(Label);
        if (marquee && this.marqueeText) marquee.string = this.marqueeText;
    };

    /** 启动阶段就失败（Bundle 或 config 取不到）：这时还没有任何权威数值可保留。 */
    private onFatal(err: unknown): void {
        const code = err instanceof ApiError ? err.code : "UNKNOWN";
        const label = this.node.getChildByPath("Hud/CoinLabel")?.getComponent(Label);
        if (label) label.string = `读取失败 ${code}`;
        console.error("[m05] Mall 启动失败", err);
    }

    /**
     * 轮询节拍。间隔跟配置走而不是写死 5：
     * B3.6 的 5 秒与 visitorIntervalSeconds 初版同值，ADR 0001 的要求是「将请求量与结算间隔对齐」，
     * 所以调数值时两边应当一起动。下界由服务端保证（`internal/game/config.go:77` 要求 1–3600）。
     */
    private startPolling(intervalSeconds: number): void {
        this.schedule(this.onTick, intervalSeconds);
        console.log(`[m05] 开始轮询，每 ${intervalSeconds} 秒一次`);
    }

    private onTick = (): void => {
        this.store?.tick();
    };

    /**
     * 切后台停轮询、回前台立即取权威状态（B3.6）。
     * 引擎收到平台 hide 时自己也会 pauseByEngine 停主循环，所以这里只管"还要不要发请求"。
     * ⚠️ 引擎文档明写 WEB 平台这两个事件不保证 100% 触发（`cc.d.ts` 里 Game.EVENT_HIDE 的注释），
     *    因此真机/开发者工具的复验才算结案，见 docs/M05-经营闭环.md §9-⑤。
     */
    private bindLifecycle(): void {
        game.on(Game.EVENT_HIDE, this.onGameHide, this);
        game.on(Game.EVENT_SHOW, this.onGameShow, this);
    }

    private onGameHide = (): void => {
        this.store?.pause();
    };

    private onGameShow = (): void => {
        void this.store?.resume();
    };

    // ---------- 开店交互 ----------

    /**
     * M05 的最小开店入口：点「待开业」铺位直接开店。
     * 依据《系统-经营与成长》§2.4——开店不花金币、幂等，没有需要玩家确认的代价，
     * 所以不做确认弹窗。设计稿里的正式入口（经营页金色「开业」按钮）属 M06。
     */
    private bindSlotInput(cfg: Config): void {
        for (const slot of cfg.slots) {
            const found = this.findSlot(slot.id);
            if (!found) {
                console.error(`[m05] 场景里找不到铺位节点 ${slot.id}，无法绑定点击`);
                continue;
            }
            found.root.on(Node.EventType.TOUCH_END, () => this.onSlotTouched(slot), this);
        }
    }

    private onSlotTouched(slot: SlotConfig): void {
        const view = this.store?.view;
        if (!view) {
            console.log(`[m05] 铺位 ${slot.id} 被点击，但快照尚未就绪，忽略`);
            return;
        }
        const state = this.slotStateOf(slot, view);
        if (state !== "S2") {
            // S1 的解锁入口属 M06 布局页，S3 的店铺详情属 M06；M05 不做提示 UI，只留痕。
            console.log(`[m05] 铺位 ${slot.id} 当前是 ${state}，M05 只在「待开业」上响应开店`);
            return;
        }
        void this.store?.prepare(slot.shopId);
    }

    // ---------- 客人表演 ----------

    /**
     * 建表演层。四张图缺任何一张就整层不启用——
     * 缺件的客人会画成一个空节点，比不画更难排查；素材齐了才上场，且缺件日志已在预加载时出过。
     */
    private buildGuestStage(cfg: Config): void {
        const layer = this.node.getChildByPath("GuestLayer");
        if (!layer) {
            console.error("[m05] 场景里没有 GuestLayer，客人无法上场");
            return;
        }
        const pick = (key: keyof GuestArt): SpriteFrame | null => {
            const [bundleName, framePath] = GUEST_ART[key];
            return this.frames.get(frameKey(bundleName, framePath)) ?? null;
        };
        const picked: Record<keyof GuestArt, SpriteFrame | null> = {
            side: pick("side"),
            front: pick("front"),
            back: pick("back"),
            coin: pick("coin"),
        };
        const missing = (Object.keys(picked) as Array<keyof GuestArt>).filter((key) => !picked[key]);
        if (missing.length) {
            console.error(`[m05] 表演层缺图 ${missing.join("/")}，本轮不启用客人走位`);
            return;
        }

        const slots = new Map<string, Node>();
        for (const slot of cfg.slots) {
            const found = this.findSlot(slot.id);
            if (found) slots.set(slot.shopId, found.root);
        }
        this.guests = new GuestStage(layer, picked as GuestArt, slots);
        console.log(`[m05] 表演层就绪，客人可映射到 ${slots.size} 家店铺`);
    }

    // ---------- 资源 ----------

    /**
     * 取三态着色器。取不到**不阻断渲染**：铺位仍要靠 Veil/Lock/文字表达状态，
     * 只是少了压暗那一档，所以只记日志不抛错。
     */
    private loadSlotEffect(): Promise<void> {
        const bundle = this.bundles.get(UI_BUNDLE);
        if (!bundle) return Promise.resolve();
        return new Promise((resolve) => {
            bundle.load(EFFECT_PATH, EffectAsset, (err, asset) => {
                if (err || !asset) {
                    console.error(`[m05] 三态着色器 ${EFFECT_PATH} 取不到，铺位将不压暗`, err);
                } else {
                    this.effect = asset;
                }
                resolve();
            });
        });
    }

    /** Bundle 加载失败不许白屏：铺位仍要有文字状态可读，所以只记日志、由调用侧决定降级。 */
    private loadBundle(name: string): Promise<Bundle> {
        return new Promise((resolve, reject) => {
            assetManager.loadBundle(name, (err, bundle) => {
                if (err || !bundle) {
                    console.error(`[m05] Bundle ${name} 加载失败`, err);
                    reject(new ApiError("BUNDLE_LOAD_FAILED", 0));
                    return;
                }
                resolve(bundle);
            });
        });
    }

    /**
     * 启动时一次性取回全部会用到的帧，之后渲染纯同步。
     * 清单 = 常驻件 + 遮挡件 + 每个铺位的「立绘」与「该层空铺底图」（三态覆盖完）。
     */
    private preloadFrames(cfg: Config): Promise<void> {
        const wanted: Array<readonly [string, string]> = [];
        for (const [, bundleName, framePath] of STATIC_ART) wanted.push([bundleName, framePath]);
        for (const [, framePath] of SLOT_OVERLAY) wanted.push([UI_BUNDLE, framePath]);
        for (const slot of cfg.slots) {
            wanted.push([ART_BUNDLE, shopArtPath(slot.shopId)]);
            wanted.push([ART_BUNDLE, emptyArtPath(slot.floor)]);
        }
        for (const key of Object.keys(GUEST_ART) as Array<keyof GuestArt>) {
            wanted.push(GUEST_ART[key]);
        }
        return Promise.all(wanted.map(([bundleName, framePath]) => this.loadFrame(bundleName, framePath)))
            .then(() => undefined);
    }

    private loadFrame(bundleName: string, framePath: string): Promise<void> {
        const key = frameKey(bundleName, framePath);
        if (this.frames.has(key)) return Promise.resolve();
        const bundle = this.bundles.get(bundleName);
        if (!bundle) return Promise.resolve();
        return new Promise((resolve) => {
            bundle.load(framePath, SpriteFrame, (err, frame) => {
                if (err || !frame) {
                    // 素材目前是占位图，真素材替换前缺件属预期；这里必须出声，否则以后无从排查。
                    console.error(`[m05] ${bundleName} 内取不到 ${framePath}`, err);
                } else {
                    this.frames.set(key, frame);
                }
                resolve();
            });
        });
    }

    /** 从缓存取帧挂到指定路径的 Sprite 上。缺件在预加载时已出过声，这里不重复刷日志。 */
    private setFrame(nodePath: string, bundleName: string, framePath: string): void {
        const sprite = this.node.getChildByPath(nodePath)?.getComponent(Sprite);
        if (!sprite) {
            console.error(`[m05] 场景里找不到带 Sprite 的节点 ${nodePath}`);
            return;
        }
        const frame = this.frames.get(frameKey(bundleName, framePath));
        if (frame) sprite.spriteFrame = frame;
    }

    private applyStaticArt(): void {
        for (const [nodePath, bundleName, framePath] of STATIC_ART) {
            this.setFrame(nodePath, bundleName, framePath);
        }
    }

    // ---------- 渲染 ----------

    /** 一次幂等重渲染：同一份快照渲染两次结果一致，且不触发任何异步加载。 */
    private render(view: MallView): void {
        this.renderSlots(view);
        this.renderHud(view);
    }

    private renderSlots(view: MallView): void {
        if (!this.config) return;
        for (const slot of this.config.slots) {
            const found = this.findSlot(slot.id);
            if (!found) {
                console.error(`[m05] 场景里找不到铺位节点 ${slot.id}`);
                continue;
            }
            const { root, path } = found;
            const shop = view.shops.find((s) => s.id === slot.shopId) ?? null;
            const state = this.slotStateOf(slot, view);

            // S3 换成立绘；S1/S2 都显示该楼层的空铺底图（B3.5，也是 M04 验收第 3 条）。
            this.setFrame(
                `${path}/Art`,
                ART_BUNDLE,
                state === "S3" ? shopArtPath(slot.shopId) : emptyArtPath(slot.floor),
            );
            for (const [child, overlayPath] of SLOT_OVERLAY) {
                this.setFrame(`${path}/${child}`, UI_BUNDLE, overlayPath);
                const overlay = root.getChildByName(child);
                if (overlay) overlay.active = state === "S1";
            }
            this.applyEmissive(root, state === "S1" ? EMISSIVE_DIM : EMISSIVE_FULL);

            const tag = root.getChildByName("Tag")?.getComponent(Label);
            if (tag) tag.string = this.tagOf(slot, view, state, shop);
        }
    }

    /**
     * HUD 五槽：金币图标 + 金币数值 + 3 个导航按钮 + 客流。数值全部来自服务端。
     * 用滚动而非直接赋值（《美术圣经》第 848 行 ≤400ms ease-out）：
     * 金币每 5 秒才涨几点，直接跳会让玩家以为是界面自己在动。
     */
    private renderHud(view: MallView): void {
        const coin = this.node.getChildByPath("Hud/CoinLabel")?.getComponent(Label);
        if (coin) rollNumber(coin, view.coins);

        const visitor = this.node.getChildByPath("Hud/VisitorLabel")?.getComponent(Label);
        if (visitor) {
            // dailyVisitorCap 为 null 表示当天还没冻结，显示 — 而不是假的 0（与 Boot 同口径）。
            const cap = view.dailyVisitorCap === null ? "—" : String(view.dailyVisitorCap);
            rollNumber(visitor, view.visitorsServed, (n) => `${n}/${cap}`);
        }
    }

    private slotStateOf(slot: SlotConfig, view: MallView): SlotState {
        if (view.unlockedSlots.indexOf(slot.id) < 0) return "S1";
        const shop = view.shops.find((s) => s.id === slot.shopId);
        return shop && shop.prepared ? "S3" : "S2";
    }

    private tagOf(slot: SlotConfig, view: MallView, state: SlotState, shop: ShopView | null): string {
        if (state === "S3" && shop) return shop.name;
        if (state === "S2") return "待开业";
        return view.nextSlotId === slot.id ? `解锁 $${view.nextUnlockCost}` : "未开放";
    }

    /** 铺位挂在两层之一，返回节点与它相对 Canvas 的路径（按路径取节点要用全路径）。 */
    private findSlot(slotId: string): { root: Node; path: string } | null {
        for (const floor of ["Floor1", "Floor2"]) {
            const path = `${floor}/${slotId}`;
            const root = this.node.getChildByPath(path);
            if (root) return { root, path };
        }
        return null;
    }

    /**
     * 三态压暗：给铺位的 Art 挂一份由 slot-emissive 着色器建出的材质并设档位。
     * **每个铺位各持一份材质实例**——共用一份会让 setProperty 相互串台，而 §10 要的是逐铺位状态。
     */
    private applyEmissive(slotRoot: Node, value: number): void {
        const sprite = slotRoot.getChildByName("Art")?.getComponent(Sprite);
        if (!sprite) return;
        if (!sprite.customMaterial) {
            if (!this.effect) return;      // 着色器没取到就保持默认材质：只是不压暗，不报错
            const mat = new Material();
            // USE_TEXTURE 必须显式给：defines 不写时引擎"默认全为 0"，材质会走不采图的分支。
            mat.initialize({ effectAsset: this.effect, defines: { USE_TEXTURE: true } });
            sprite.customMaterial = mat;
        }
        sprite.customMaterial.setProperty("u_emissive", value);
    }
}

/** S3 营业中用的店铺立绘。 */
function shopArtPath(shopId: string): string {
    return `shop/shop_${shopId}_open/spriteFrame`;
}

/** S1/S2 用的该层空铺底图。 */
function emptyArtPath(floor: number): string {
    return `slt/slt_f${floor}_empty/spriteFrame`;
}
