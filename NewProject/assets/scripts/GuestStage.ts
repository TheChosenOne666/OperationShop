// M05 客人表演层：把「这一轮到了几位客人」画成从街口走进店门、留下金币飘字再离开的过程。
//
// 规格来源（逐条对齐，不自行发挥）：
//   《玩法与技术方案》B3.5 —— 从入口沿路径走到目标店铺门口 → 停留约 0.5 秒 → 店铺位置金币飘字 → 淡出消失；
//                             按行走方向切正/侧/背三视图；金币变化用补间数字滚动，不做逐枚硬币飞行。
//   《资产基准模板与生成提示词》第 631 行 —— v1 客人只做整体位移（所以走位是一段直线，不画路径、不走楼梯）。
//   《美术圣经》第 391–395 行 —— 客人显示尺寸 64×96（源图 256×256）；第 414 行 —— 飘字金币用 ico_coin 48×48。
//
// 与预制体的关系：本项目场景与素材全走代码（见 docs/M04 §2「为什么全走路径加载」），
// 客人节点也由代码建，不落 Guest.prefab——主理人 2026-09-21 在 docs/M05-经营闭环.md §2 D3 拍板。
//
// ⚠️ 两条容易踩的实现细节：
//   1. Sprite 不设 `sizeMode = CUSTOM` 会按源图 256×256 显示，客人会大到盖住半个铺位；
//   2. 淡出要挂在 UIOpacity 上而不是 Node 上——Node 没有 opacity，tween 一个不存在的属性不报错但不动。
import { Label, Node, Sprite, SpriteFrame, Tween, tween, UIOpacity, UITransform, Vec3 } from "cc";
import type { Arrival, StoreUpdate } from "./MallStore";

/** 客人显示尺寸（美术圣经 §4.5：源图 256×256，显示 64×96）。 */
const GUEST_WIDTH = 64;
const GUEST_HEIGHT = 96;
/** 飘字金币图标 48×48（美术圣经第 414 行）。 */
const COIN_ICON = 48;

/** 街口在 GuestLayer 本地坐标的 x：图层宽 750，从左侧外缘外走进来。 */
const ENTRY_MARGIN = 64;
/** 一次表演的时长分配。走位 1.6s + 停留 0.5s + 淡出 0.3s ≈ 2.4s，短于 5 秒轮询间隔，
 *  所以不会出现"上一位还没走完下一位就来"的堆积；停留 0.5s 是 B3.5 的原文值。 */
const WALK_SECONDS = 1.6;
const STAY_SECONDS = 0.5;
const FADE_SECONDS = 0.3;
/** 同一轮多位客人之间的起步错开量，避免完全重叠成一个人。 */
const STAGGER_SECONDS = 0.35;
/** 飘字上浮距离。 */
const FLOAT_RISE = 60;

/**
 * 单轮最多画几位客人。回前台时服务端可能一次补算几十位（客流按真实时间结转），
 * 全放出来既卡又看不清。**只降级表现，不降级数值**——金币与客流仍按服务端值滚动到位。
 * 阈值由主理人 2026-09-21 在 docs/M05-经营闭环.md §9-③ 定为 5。
 */
const MAX_GUESTS_PER_UPDATE = 5;

/** 表演层要用的四张图，由 MallScene 预加载后注入（这里不碰 Bundle，保持可单测）。 */
export interface GuestArt {
    side: SpriteFrame;
    front: SpriteFrame;
    back: SpriteFrame;
    coin: SpriteFrame;
}

export class GuestStage {
    private readonly layer: Node;
    private readonly art: GuestArt;
    /** shopId → 该店所在铺位节点，用来算"店门口"在哪。 */
    private readonly shopSlots: Map<string, Node>;

    constructor(layer: Node, art: GuestArt, shopSlots: Map<string, Node>) {
        this.layer = layer;
        this.art = art;
        this.shopSlots = shopSlots;
    }

    /**
     * 按本轮到店归属生成客人。arrivals 来自 MallStore 对服务端累计计数做的差，
     * 本层不做任何数值推算（ADR 0001）。
     */
    spawn(update: StoreUpdate): void {
        const queue: Array<{ slot: Node; unitPrice: number }> = [];
        for (const arrival of update.arrivals) {
            const slot = this.shopSlots.get(arrival.shopId);
            const shop = update.view.shops.find((s) => s.id === arrival.shopId);
            if (!slot) {
                console.error(`[m05] 找不到店铺 ${arrival.shopId} 对应的铺位节点，本轮 ${arrival.visitors} 位客人无处可去`);
                continue;
            }
            if (!shop) {
                console.error(`[m05] 快照里没有店铺 ${arrival.shopId}，无法取单价做飘字`);
                continue;
            }
            for (let i = 0; i < arrival.visitors; i += 1) {
                queue.push({ slot, unitPrice: shop.unitPrice });
            }
        }
        if (queue.length === 0) return;

        const shown = queue.slice(0, MAX_GUESTS_PER_UPDATE);
        if (queue.length > shown.length) {
            console.log(`[m05] 本轮到店 ${queue.length} 位，只播 ${shown.length} 位（表现降级，数值仍按服务端滚动到位）`);
        }
        shown.forEach((guest, index) => this.startGuest(guest.slot, guest.unitPrice, index * STAGGER_SECONDS));
    }

    /** 场景销毁时调用：停掉所有补间并清场，避免回调打到已销毁的节点。 */
    dispose(): void {
        for (const child of this.layer.children.slice()) {
            Tween.stopAllByTarget(child);
            const opacity = child.getComponent(UIOpacity);
            if (opacity) Tween.stopAllByTarget(opacity);
            child.destroy();
        }
    }

    /** 当前在场人数，供日志与验证核对。 */
    get onStage(): number {
        return this.layer.children.length;
    }

    private startGuest(slot: Node, unitPrice: number, delay: number): void {
        const door = this.doorOf(slot);
        const guest = this.makeGuest();
        const entryX = -(this.layer.getComponent(UITransform)!.width / 2 + ENTRY_MARGIN);
        guest.setPosition(entryX, 0, 0);

        const sprite = guest.getComponent(Sprite)!;
        // 行进方向决定侧视图朝向。⚠️ 3.8 的 Sprite 没有 flipX（那是 2.x 的 API），
        // 能翻的是 SpriteFrame.flipUVX——但那是**资产级**的，一改就把所有共用这张帧的客人全翻了。
        // 逐节点翻转只能用负 scale。
        guest.setScale(door.x < entryX ? -1 : 1, 1, 1);

        tween(guest)
            .delay(delay)
            .to(WALK_SECONDS, { position: new Vec3(door.x, door.y, 0) }, { easing: "linear" })
            .call(() => {
                // 到门口转身面向玩家（B3.5「按行走方向切换三视图」）
                guest.setScale(1, 1, 1);
                sprite.spriteFrame = this.art.front;
            })
            .delay(STAY_SECONDS)
            .call(() => {
                this.spawnCoinFloat(door, unitPrice);
                // 买完转身离开：淡出前换背视图
                sprite.spriteFrame = this.art.back;
                this.fadeOut(guest);
            })
            .start();
    }

    /**
     * 「店门口」= 铺位节点位置换算到客人层，再落到立绘下缘略往里一点。
     * 用运行时坐标换算而不是写死数字：换设备/换适配时铺位位置会变，硬编码会走位错位。
     */
    private doorOf(slot: Node): Vec3 {
        const layerUi = this.layer.getComponent(UITransform)!;
        const local = layerUi.convertToNodeSpaceAR(slot.getWorldPosition());
        const artHeight = slot.getChildByName("Art")?.getComponent(UITransform)?.height ?? 0;
        local.y -= artHeight / 2 - GUEST_HEIGHT / 4;
        return local;
    }

    private makeGuest(): Node {
        const node = new Node("Guest");
        this.layer.addChild(node);
        node.addComponent(UITransform).setContentSize(GUEST_WIDTH, GUEST_HEIGHT);
        const sprite = node.addComponent(Sprite);
        // 不设 CUSTOM 会按源图 256×256 显示，客人会大到盖住半个铺位
        sprite.sizeMode = Sprite.SizeMode.CUSTOM;
        sprite.spriteFrame = this.art.side;
        node.addComponent(UIOpacity);
        return node;
    }

    /** 店铺位置的金币飘字：ico_coin + 「+单价」。单价直接取服务端字段，不做乘法（ADR 0001）。 */
    private spawnCoinFloat(at: Vec3, unitPrice: number): void {
        const root = new Node("CoinFloat");
        this.layer.addChild(root);
        const start = new Vec3(at.x, at.y + GUEST_HEIGHT / 2, 0);
        root.setPosition(start);
        root.addComponent(UITransform).setContentSize(COIN_ICON + 96, COIN_ICON);

        const icon = new Node("Icon");
        root.addChild(icon);
        icon.setPosition(-COIN_ICON / 2 - 20, 0, 0);
        icon.addComponent(UITransform).setContentSize(COIN_ICON, COIN_ICON);
        const iconSprite = icon.addComponent(Sprite);
        iconSprite.sizeMode = Sprite.SizeMode.CUSTOM;
        iconSprite.spriteFrame = this.art.coin;

        const text = new Node("Text");
        root.addChild(text);
        text.setPosition(34, 0, 0);
        const label = text.addComponent(Label);
        label.string = `+${unitPrice}`;
        label.fontSize = 30;
        label.lineHeight = 34;

        const opacity = root.addComponent(UIOpacity);
        tween(root).to(0.8, { position: new Vec3(start.x, start.y + FLOAT_RISE, 0) }, { easing: "quadOut" }).start();
        tween(opacity)
            .delay(0.4)
            .to(0.4, { opacity: 0 })
            .call(() => {
                if (root.isValid) root.destroy();
            })
            .start();
    }

    /** 淡出挂在 UIOpacity 上：Node 没有 opacity，tween 不存在的属性不报错但画面不动。 */
    private fadeOut(node: Node): void {
        const opacity = node.getComponent(UIOpacity);
        if (!opacity) {
            node.destroy();
            return;
        }
        tween(opacity)
            .to(FADE_SECONDS, { opacity: 0 })
            .call(() => {
                if (node.isValid) node.destroy();
            })
            .start();
    }
}
