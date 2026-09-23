// M06 弹窗层：店铺详情（稿 04）、解锁确认（稿 05）、升级确认（稿 06）。
//
// 为什么要有这三道弹窗（docs/M06-页面与弹窗.md §1）：解锁 600~1200、升级 300~2000 是游戏里
// 两笔最大额的**不可逆**支出，而 M05 的界面是「点铺位直接发命令」——误触即扣钱且撤不回来。
// 弹窗的唯一职责就是把「花多少、还剩多少、之后变成什么」在扣钱**之前**讲清楚。
//
// 三条实现约束（与 MallPages 同源）：
//   1. **渲染同步且幂等**：三屏节点在构造时一次建好并全部隐藏，渲染只改字符串、显隐与重画。
//      渲染路径上不引异步（回调会在多轮之间堆积、顺序不可控）。
//   2. **确认后只发命令，不本地改数**：本文件不碰 transport，点「确认」只调用 handlers；
//      数字变化一律等 MallStore 把响应里的新快照交回来（ADR 0001）。收到回执前保持
//      `pending`，重复点击不出第二条命令。
//   3. **数值只读服务端**：允许出现的运算只有两种——服务端字段之间**不含规则常量**的加减
//      （差额 `cost − coins`、余额预览 `coins − cost`、升级后的基础价 `nextUnitPrice − floorBonus`、
//      单价提升 `nextUnitPrice − unitPrice`；边界由主理人 2026-09-22 裁定，见 docs/M06 §8 D-4
//      与 ADR 0001 的纯算术一节），以及逐层「已开业数 / 铺位数」的纯计数（GDD §6.1 明文许可）。
//      单价表、解锁价、升级价、满铺值本身一个都不许出现在这里。
//
// 两条硬边界（踩了就是验收不过）：
//   · 架构现状 §8-8：`unitPrice` / `upgradeCost` 对**未解锁、未开业**的铺位也照常返回。
//     详情弹窗不得因为"字段有值"就渲染成可升级——按 `prepared` 与 `upgradeCost` 自行置灰。
//   · 验收第 3 条：累计客流 / 累计收益逐字取服务端原值，**不许**用「客流 × 单价」现推
//     （单价会变，分段的存在理由就是这笔账不能现推）。
//
// 满级态（主理人 2026-09-22 裁定 D-5）：等级徽章 `Lv5 MAX`、隐藏升级预告两行、
// 主按钮收成禁用态「已达最高等级」。
// 金币不足态（稿 04 右图，三重编码）：① 形状=锁形角标 ② 文字=「再攒 N 金币」（写出差多少，
// 不写"金币不足"）③ 明度=不透明度 78%。**禁止变红**（红=危险，违反美术圣经 §9.2 无失败态）。
//
// 稿没画的形态，按「同一套编码往下推」处理并登记在 docs/M06 §10.3：
//   · 待开业铺位的详情：状态写「待开业」，主按钮禁用、文字「开业后才能升级」——服务端对未开业
//     店铺升级会返回 SHOP_NOT_OPEN，把它画成可点就是骗玩家。
//   · 解锁确认只有「可解锁」一种形态（稿 05 明示：入口在布局页主按钮，金币不足时那颗按钮本身
//     已禁用，不给进弹窗的路），所以本弹窗不做禁用态。
import { BlockInputEvents, Graphics, Label, Node, Sprite, SpriteFrame, UIOpacity, UITransform } from "cc";
import type { Config, MallView, ShopView, SlotConfig } from "./ApiTypes";
import {
    C, DESIGN_HEIGHT, DESIGN_WIDTH, FONT, type SlotState,
    arrowRight, container, disc, label, lockBadge, lockChip, paint, paintVeil, place, plusBadge, rect, rounded, rgb,
    strokePath, withAlpha,
} from "./MallTheme";

/** 栈里可能出现的弹窗。detail 是页面，unlock / upgrade 是它的两道确认。 */
export type PopupName = "detail" | "unlock" | "upgrade";

/** 弹窗上报给场景的动作。与页面层一样只报意图，不碰传输层（ADR 0001）。 */
export interface PopupsHandlers {
    /** 详情弹窗的「升级到 Lv(n+1)」→ 升级确认弹窗。这一页不直接提交升级（稿 04 交互说明）。 */
    onAskUpgrade(shopId: string): void;
    /** 解锁确认弹窗的「确认解锁」→ 提交 `POST /api/v1/slots/{slotId}/unlock`。 */
    onConfirmUnlock(slotId: string): void;
    /** 升级确认弹窗的「确认升级」→ 提交 `POST /api/v1/shops/{shopId}/upgrade`。 */
    onConfirmUpgrade(shopId: string): void;
}

export interface PopupsDeps {
    /** 挂遮罩的节点（本场景即 Canvas，750×1334）。必须**在页面层之后**创建，才盖得住页面。 */
    root: Node;
    config: Config;
    /** 与 MallScene 共用帧缓存；取不到返回 null，调用侧保持不画而不是画成色块。 */
    frame: (bundleName: string, framePath: string) => SpriteFrame | null;
    handlers: PopupsHandlers;
}

// ---------- 稿值几何（@750×1334） ----------

/** 面板宽 630、圆角 22、描边 6、内边距 26（三屏的「尺寸要点」逐字一致）。 */
const PANEL_WIDTH = 630;
/** 外沿到奶白板的一条边：描边 6 + 内边距 26。 */
const PANEL_INSET = 32;
const PANEL_RADIUS = 22;
const PANEL_BORDER = 6;
/** 奶白板：描边 4、圆角 14；内边距稿 04 是 20、稿 05/06 是 22。 */
const INNER_BORDER = 4;
const INNER_RADIUS = 14;
/** 按钮高 104（≥88 触控区）、两钮等分宽、间距 14。 */
const BUTTON_HEIGHT = 104;
const BUTTON_GAP = 14;
/** 关闭钮直径 64，压在面板右上角外沿（top / right 各 −18）。 */
const CLOSE_SIZE = 64;
const CLOSE_OVERHANG = 18;
/** 遮罩 rgba(74,47,30,.58)：#4A2F1E 就是 C.woodLine，不另开一份色值。 */
const SCRIM_ALPHA = 0.58;
/** 禁用态不透明度 78%（仍 ≥3:1，不是"灰到看不见"）。 */
const DISABLED_OPACITY = Math.round(255 * 0.78);
/** 角标直径：稿 04 的按钮锁标 52，页面层主按钮用 56，这里跟稿。 */
const CORNER_SIZE = 52;

/** 数据行：正文 26 + 上下内边距 9 = 44；分隔线 3 高 + 上下 8 = 19。 */
const KV_HEIGHT = 44;
const DIVIDER_BLOCK = 19;
/** 金币账的行高：正文 25 + 上下 5 = 35；盒子 = 三行 105 + 上下 14 + 描边 6 = 139。 */
const MONEY_ROW = 35;
const MONEY_HEIGHT = 139;

/** 详情弹窗被隐藏的「升级预告」整块高度：分隔线 19 + 两行 44×2 = 107。 */
const PREVIEW_BLOCK = DIVIDER_BLOCK + KV_HEIGHT * 2;
/** 详情弹窗按钮行在内容区里的 top：48+16+190+16+44+10+60+10+19+44+44+19+44+44+18 = 626。 */
const DETAIL_BUTTONS_TOP = 626;
/** 店面示意 518×190（稿 04 的 .art），前后对照缩略 243×180（稿 05 的 .slotbox ~275×180，按 514 内容宽等分）。 */
const ART_HEIGHT = 190;
const PAIR_BOX = { width: 243, height: 180 };

const TOP = { detail: 250, unlock: 230, upgrade: 250 } as const;
const PAD = { detail: 20, unlock: 22, upgrade: 22 } as const;
/**
 * 三屏各自的内容高 = 逐行相加，末行下缘即内容高；面板下缘必须停在
 * HUD 上方禁放区（1334 − 160 − 53 = 1121）之内。算式：
 *   · 详情 626 + 104 = 730 → 面板高 842 → 下缘 1092
 *   · 解锁 647 + 104 = 751 → 面板高 867 → 下缘 1097
 *   · 升级 593 + 104 = 697 → 面板高 813 → 下缘 1063
 */
const CONTENT_HEIGHT = { detail: 730, unlock: 751, upgrade: 697 } as const;

/** 内容宽 = 630 − 2×32 − 2×(内边距 + 描边 4)。详情 518，另两屏 514。 */
function contentWidth(name: PopupName): number {
    return PANEL_WIDTH - PANEL_INSET * 2 - (PAD[name] + INNER_BORDER) * 2;
}

/** 按钮的三种长相。次级靠"更暗、无金晕"表达，不靠降低文字对比度（稿 04 注释）。 */
type ButtonLook = "primary" | "ghost" | "disabled";

interface PopupButton {
    node: Node;
    /** 按钮自己的 Graphics（每节点一个，重画必须走 paintButton 的同一道 clear）。 */
    graphics: Graphics;
    /** 右上角标（加号 / 锁），形状通道。 */
    corner: Node;
    text: Label;
    opacity: UIOpacity;
    width: number;
}

/** 弹窗里一左一右两列的一行。 */
interface KvRow {
    key: Label;
    value: Label;
}

interface DetailCells {
    panel: Node;
    inner: Node;
    content: Node;
    close: Node;
    name: Label;
    badgeText: Label;
    statusText: Label;
    statusTick: Node;
    art: Sprite;
    veil: Graphics;
    price: KvRow;
    formula: Label;
    visitors: KvRow;
    revenue: KvRow;
    preview: Node;
    previewLevel: KvRow;
    previewCost: KvRow;
    buttons: Node;
    primary: PopupButton;
    ghost: PopupButton;
}

interface UnlockCells {
    panel: Node;
    subtitle: Label;
    beforeArt: Sprite;
    beforeVeil: Graphics;
    afterArt: Sprite;
    cost: Label;
    coins: Label;
    balance: Label;
    lines: Label[];
    primary: PopupButton;
    ghost: PopupButton;
}

interface UpgradeCells {
    panel: Node;
    title: Label;
    fromBadge: Label;
    toBadge: Label;
    nowValue: Label;
    nowNote: Label;
    nextValue: Label;
    nextNote: Label;
    delta: Label;
    cost: Label;
    coins: Label;
    balance: Label;
    rule: Label;
    primary: PopupButton;
    ghost: PopupButton;
}

export class MallPopups {
    private readonly deps: PopupsDeps;
    private readonly overlay: Node;
    private readonly notice: Node;
    private readonly noticeText: Label;
    private readonly detail: DetailCells;
    private readonly unlock: UnlockCells;
    private readonly upgrade: UpgradeCells;
    private readonly panels: Record<PopupName, Node>;
    /** 打开顺序，末元素是最上面那层。详情可以垫在确认弹窗底下，取消就退回它。 */
    private readonly stack: PopupName[] = [];
    private readonly subject: Record<PopupName, string | null> = { detail: null, unlock: null, upgrade: null };
    private readonly lastLog: Record<PopupName, string> = { detail: "", unlock: "", upgrade: "" };
    /** 最近一次渲染用的快照。点击要现地重算禁用态，不靠入口那次判断（验收第 5 条）。 */
    private view: MallView | null = null;
    /** 打开升级确认时的等级，用来在日志里说清「这一笔是从哪级升上来的」。 */
    private upgradeFromLevel = 0;
    /** 已提交写命令、还没收到回执。为真时再点确认不出第二条命令。 */
    private pending = false;

    constructor(deps: PopupsDeps) {
        this.deps = deps;
        this.overlay = new Node("Overlay");
        deps.root.addChild(this.overlay);
        this.overlay.layer = deps.root.layer;
        this.overlay.addComponent(UITransform).setContentSize(DESIGN_WIDTH, DESIGN_HEIGHT);
        this.overlay.setPosition(0, 0, 0);

        // 遮罩：既压暗下层，也吃掉下层的一切点击（与 MallPages 的约束 3 同源）。
        const scrim = new Node("Scrim");
        this.overlay.addChild(scrim);
        scrim.layer = this.overlay.layer;
        place(scrim, 0, 0, DESIGN_WIDTH, DESIGN_HEIGHT);
        paint(scrim, (g) => rect(g, DESIGN_WIDTH, DESIGN_HEIGHT, withAlpha(C.woodLine, SCRIM_ALPHA)));
        scrim.addComponent(BlockInputEvents);
        scrim.on(Node.EventType.TOUCH_END, () => this.closeTop(), scrim);

        // 提示条落在面板上方的遮罩区。稿上没有失败态，塞进面板就得为三屏各预留一行空白；
        // 放在这里既盖得住，又不动稿上的几何（验收第 8 条只要求"提示看得见"）。
        this.notice = container(this.overlay, "Notice", 95, 150, 560, 56);
        paint(this.notice, (g) => rounded(g, 560, 56, 12, rgb(C.woodLine)));
        this.noticeText = label(this.notice, "Text", "", FONT.body, C.creamBright, 12, 8, 536, 40, false);
        this.notice.active = false;

        this.detail = this.buildDetail();
        this.unlock = this.buildUnlock();
        this.upgrade = this.buildUpgrade();
        this.panels = {
            detail: this.detail.panel,
            unlock: this.unlock.panel,
            upgrade: this.upgrade.panel,
        };
        for (const node of Object.values(this.panels)) node.active = false;
        // ⚠️ 整层默认收起：遮罩带 BlockInputEvents，留着 active 就等于给全场景盖了一层
        // 吃不掉点击的玻璃——弹窗一次都没开过，主界面与页面就全都点不动了。
        this.overlay.active = false;
    }

    /** 最上面那层的名字，供日志与驱动器核对；没开就是 null。 */
    get current(): PopupName | null {
        return this.stack.length ? this.stack[this.stack.length - 1] : null;
    }

    // ---------- 开关 ----------

    /** 店铺详情（稿 04）。入口：营业中 / 待开业的铺位（主界面 S3、经营页行、布局页卡）。 */
    openDetail(shopId: string): void {
        if (!this.view) {
            console.log(`[m06] 弹窗未打开：还没有任何服务端快照（${shopId}）`);
            return;
        }
        this.subject.detail = shopId;
        this.push("detail");
        this.renderDetail(this.view);
        console.log(`[m06] 弹窗打开 详情 ${shopId}`);
    }

    /**
     * 解锁确认（稿 05）。⚠️ 只服务于「下一个可解锁铺位」——这是稿 05 明示的有意约束：
     * 入口在布局页主按钮，金币不足时那颗按钮本身就是禁用态，不给进弹窗的路。
     * 所以对 `nextSlotId` 断言：传进来别的铺位就是接线错了，出声并且不开。
     */
    openUnlock(slotId: string): void {
        const view = this.view;
        if (!view) {
            console.log(`[m06] 弹窗未打开：没有快照（${slotId}）`);
            return;
        }
        if (view.nextSlotId !== slotId) {
            console.error(`[m06] 解锁确认只接受下一个可解锁铺位 ${view.nextSlotId}，收到 ${slotId}，不开弹窗`);
            return;
        }
        this.subject.unlock = slotId;
        this.push("unlock");
        this.renderUnlock(view);
        console.log(`[m06] 弹窗打开 解锁确认 ${slotId} 花费 ${view.nextUnlockCost}`);
    }

    /** 升级确认（稿 06）。只从详情弹窗的主按钮进来，所以进到这里的一定"看着可升级"。 */
    openUpgrade(shopId: string): void {
        const view = this.view;
        const shop = view?.shops.find((s) => s.id === shopId);
        if (!view || !shop) {
            console.log(`[m06] 弹窗未打开：没有快照或快照里没有 ${shopId}`);
            return;
        }
        if (shop.upgradeCost === null || shop.nextUnitPrice === null) {
            console.log(`[m06] ${shopId} 已是最高等级，不出升级确认弹窗`);
            return;
        }
        this.subject.upgrade = shopId;
        this.upgradeFromLevel = shop.level;
        this.push("upgrade");
        this.renderUpgrade(view);
        console.log(`[m06] 弹窗打开 升级确认 ${shopId} Lv${shop.level}→Lv${shop.level + 1} 花费 ${shop.upgradeCost}`);
    }

    /** 关掉最上面一层（点遮罩、点关闭钮、点「再想想」/「取消」都走这条，不做二次确认）。 */
    closeTop(): void {
        if (!this.stack.length) return;
        const top = this.stack.pop() as PopupName;
        this.pending = false;
        this.sync();
        this.refreshTop();
        console.log(`[m06] 弹窗关闭 ${top}（取消，不发任何写命令）`);
    }

    /** 一次关掉整层。数据不再成立时用（例如快照里找不到这家店了）。 */
    closeAll(): void {
        if (!this.stack.length) return;
        this.stack.length = 0;
        this.pending = false;
        this.sync();
        console.log("[m06] 弹窗整层关闭");
    }

    /**
     * 写命令的回执：服务端已接受，场景**先用响应里的新快照 render() 完**再调这里。
     * 只关确认弹窗，详情留在原地显示升级后的新档位——玩家当场看得见数字变了，
     * 详情也就不用关掉再开一次（验收第 4 条要的就是这种可对账的现场）。
     */
    acknowledgeWrite(): void {
        if (!this.pending) return;
        this.pending = false;
        const top = this.stack[this.stack.length - 1];
        if (top === "unlock" || top === "upgrade") {
            this.stack.pop();
            this.sync();
            this.refreshTop();
            console.log(`[m06] 弹窗回执 ${top} 已生效，关掉确认层`);
        }
    }

    /** 写命令被拒：弹窗留着（数字没变、玩家还在现场），但允许再点一次。 */
    rejectWrite(): void {
        if (!this.pending) return;
        this.pending = false;
        console.log("[m06] 写命令被拒，确认按钮恢复可点（界面数值仍是最后一次成功值）");
    }

    /**
     * 弹窗内的提示。文案由 `MallErrors.errorText(code)` 给，本文件不碰错误码。
     * 没弹窗开着时不抢页面自己的提示位——那一路由 MallPages.notify 负责。
     */
    notify(text: string | null): void {
        if (!this.stack.length) return;
        this.noticeText.string = text ?? "";
        this.notice.active = Boolean(text);
    }

    dispose(): void {
        if (this.overlay.isValid) this.overlay.destroy();
    }

    // ---------- 渲染 ----------

    /**
     * 一次幂等重渲染。栈里最多两层（详情 + 确认），**两层都渲染**：
     * 确认层关掉后露出的就是详情，那时它的字符串必须已经是新快照的值。
     */
    render(view: MallView): void {
        this.view = view;
        if (!this.stack.length) return;
        for (const name of this.stack) {
            if (name === "detail") this.renderDetail(view);
            else if (name === "unlock") this.renderUnlock(view);
            else this.renderUpgrade(view);
        }
    }

    /** 确认层关掉后把露出来的那层重画一次（回执与取消两条路都走这里）。 */
    private refreshTop(): void {
        const view = this.view;
        const top = this.stack[this.stack.length - 1];
        if (!view || !top) return;
        if (top === "detail") this.renderDetail(view);
        else if (top === "unlock") this.renderUnlock(view);
        else this.renderUpgrade(view);
    }

    /** 详情弹窗：状态、店面、单客收益构成、累计两行、升级预告、两颗按钮。 */
    private renderDetail(view: MallView): void {
        const cells = this.detail;
        const width = contentWidth("detail");
        const shopId = this.subject.detail;
        const shop: ShopView | undefined = shopId ? view.shops.find((s) => s.id === shopId) : undefined;
        const slot = this.slotOfShop(shopId);
        if (!shop || !slot || view.unlockedSlots.indexOf(slot.id) < 0) {
            // 铺位没有"再锁回去"的路径，走到这里只能是接线错了
            console.error(`[m06] 详情弹窗找不到已解锁铺位的数据（${shopId}），关掉弹窗`);
            this.closeAll();
            return;
        }
        const state: SlotState = shop.prepared ? "S3" : "S2";
        const maxLevel = shop.upgradeCost === null;
        const cost = shop.upgradeCost ?? 0;
        const shortfall = cost - view.coins;
        // 可升级 = 营业中 且 有下一级 且 买得起。三个条件全读服务端字段，不查单价表。
        const upgradable = shop.prepared && !maxLevel && view.coins >= cost;

        cells.name.string = shop.name;
        cells.badgeText.string = maxLevel ? `Lv${shop.level} MAX` : `Lv${shop.level}`;
        cells.statusText.string = shop.prepared ? "营业中" : "待开业";
        cells.statusTick.active = shop.prepared;
        cells.art.spriteFrame = this.deps.frame("mall_art", shop.prepared
            ? `shop/shop_${shop.id}_open/spriteFrame`
            : `slt/slt_f${slot.floor}_empty/spriteFrame`);
        paintVeil(cells.veil, width - INNER_BORDER * 2, ART_HEIGHT - INNER_BORDER * 2, state);

        cells.price.value.string = `${shop.unitPrice} 金币`;
        // 构成两半都取服务端字段（D-1 的裁定）；未满铺时服务端给 floorBonus=0，照写不藏
        cells.formula.string = `基础 ${shop.unitPriceBase} + 满铺 ${shop.floorBonus} = ${shop.unitPrice}`;
        // 两行逐字等于 shops[].visitors / .revenue，不许拿「客流 × 单价」现推（验收第 3 条）
        cells.visitors.value.string = `${shop.visitors} 位`;
        cells.revenue.value.string = `${shop.revenue} 金币`;

        // 升级预告：满级按裁定 D-5 整块隐藏；未开业同样不预告（服务端会拒 SHOP_NOT_OPEN）
        const showPreview = !maxLevel && shop.prepared;
        cells.preview.active = showPreview;
        if (showPreview) {
            const next = shop.nextUnitPrice ?? shop.unitPrice;
            cells.previewLevel.key.string = `升到 Lv${shop.level + 1} 后`;
            // (+1) 是两个服务端字段相减，属 D-4 许可的纯算术
            cells.previewLevel.value.string = `单客 ${next} 金币 (+${next - shop.unitPrice})`;
            cells.previewCost.value.string = `${cost} 金币`;
        }
        // 隐藏后把按钮块提上来，并把面板那一截空白一起收掉
        place(cells.buttons, 0, DETAIL_BUTTONS_TOP - (showPreview ? 0 : PREVIEW_BLOCK), width, BUTTON_HEIGHT);
        this.resizeDetail(showPreview ? CONTENT_HEIGHT.detail : CONTENT_HEIGHT.detail - PREVIEW_BLOCK);

        cells.ghost.text.string = "再想想";
        if (maxLevel) cells.primary.text.string = "已达最高等级";
        else if (!shop.prepared) cells.primary.text.string = "开业后才能升级";
        else if (upgradable) cells.primary.text.string = `升级到 Lv${shop.level + 1}`;
        else cells.primary.text.string = `再攒 ${shortfall} 金币`;
        // 金币不足时才用稿上的 26px（那一串比「升级到 Lv3」长）
        cells.primary.text.fontSize = maxLevel || shop.prepared ? FONT.button : 26;
        cells.primary.opacity.opacity = upgradable ? 255 : DISABLED_OPACITY;
        this.paintButton(cells.primary, upgradable ? "primary" : "disabled");
        this.paintButton(cells.ghost, "ghost");
        // 形状通道：金币不足与未开业是「被一件事挡住」→ 锁；满级是终态 → 不给锁。
        // 角标画没画同时落到节点 active 上——Graphics 里画了什么外部读不出来，驱动器只能读到这列。
        const badge: "plus" | "lock" | "none" = upgradable ? "plus" : maxLevel ? "none" : "lock";
        cells.primary.corner.active = badge !== "none";
        paint(cells.primary.corner, (g) => {
            if (badge === "plus") plusBadge(g, CORNER_SIZE, rgb(C.gold), rgb(C.woodLine), rgb(C.woodLine));
            else if (badge === "lock") lockBadge(g, CORNER_SIZE, rgb(C.veil), rgb(C.woodLine), rgb(C.woodLine));
            else g.clear();
        });

        this.log("detail", "详情", `${cells.name.string} ${cells.badgeText.string} ${cells.statusText.string}`
            + ` 单客=${cells.price.value.string}（${cells.formula.string}）`
            + ` 累计=${cells.visitors.value.string}/${cells.revenue.value.string}`
            + ` 预告=${showPreview ? `${cells.previewLevel.key.string} ${cells.previewLevel.value.string} 花费 ${cells.previewCost.value.string}` : "隐藏"}`
            + ` 主按钮=${cells.primary.text.string}`
            + `（${upgradable ? "可升级" : maxLevel ? "满级" : shop.prepared ? "金币不足" : "未开业"}`
            + `，不透明度 ${cells.primary.opacity.opacity}，角标 ${badge === "none" ? "无" : badge === "plus" ? "加号" : "锁"}）`);
    }

    /** 解锁确认：前后对照、金币账三行、解锁后会怎样三条、两颗按钮。 */
    private renderUnlock(view: MallView): void {
        const cells = this.unlock;
        const slotId = this.subject.unlock;
        const slot = this.deps.config.slots.find((s) => s.id === slotId);
        const cost = view.nextUnlockCost;
        if (!slot || cost === null || view.nextSlotId !== slot.id) {
            console.error(`[m06] 解锁确认的数据不再成立（${slotId}），关掉弹窗`);
            this.closeAll();
            return;
        }
        const name = this.shopName(slot);
        cells.subtitle.string = `${floorLabel(slot.floor)} · ${slot.position} 号铺位「${name}」`;
        // 前后刻意用同一张空铺底图，只靠纱罩 + 锁角标 + 明度区分（稿 05 的结构说明）
        const emptyPath = `slt/slt_f${slot.floor}_empty/spriteFrame`;
        cells.beforeArt.spriteFrame = this.deps.frame("mall_art", emptyPath);
        cells.afterArt.spriteFrame = this.deps.frame("mall_art", emptyPath);
        paintVeil(cells.beforeVeil, PAIR_BOX.width - INNER_BORDER * 2, PAIR_BOX.height - INNER_BORDER * 2, "S1");

        cells.cost.string = `− ${cost} 金币`;
        cells.coins.string = `${view.coins}`;
        // 余额是**确认前预览**：两个服务端字段相减（D-4 裁定允许；稿上"需服务端返回"那句由这条口径结清）
        cells.balance.string = `${view.coins - cost} 金币`;

        const { total } = this.floorProgress(view, slot.floor);
        // 稿 05 那一行「二层进度变成 2 / 3」说的是**已解锁**数（与布局页标题「n / m 已开放」同一把尺），
        // 不是已营业数——解锁只让铺位可开业，满铺要等开业之后，所以第三行不许写成"解锁后立即满铺"。
        const unlocked = this.unlockedOn(view, slot.floor) + 1;
        const remaining = total - unlocked;
        const bonus = this.deps.config.fullFloorBonus;
        const others = this.fullFloors(view).filter((f) => f !== slot.floor);
        cells.lines[0].string = `「${name}」变成可开业，可在经营页开店营业`;
        cells.lines[1].string = `${floorLabel(slot.floor)} 进度变成 ${unlocked} / ${total}`
            + (remaining > 0 ? `，还差 ${remaining} 个铺位` : `，该层铺位已全部解锁`);
        cells.lines[2].string = `${floorLabel(slot.floor)} 满铺后，该层单客收益 +${bonus}`
            + (others.length ? `（${others.map(floorLabel).join("、")}已生效）` : "");

        cells.ghost.text.string = "再想想";
        cells.primary.text.string = "确认解锁";
        cells.primary.opacity.opacity = 255;
        this.paintButton(cells.primary, "primary");
        this.paintButton(cells.ghost, "ghost");
        paint(cells.primary.corner, (g) => g.clear());
        cells.primary.corner.active = false;

        this.log("unlock", "解锁确认", `${cells.subtitle.string}`
            + ` 花费=${cells.cost.string} 现有=${cells.coins.string} 余额=${cells.balance.string}`
            + ` | ${cells.lines.map((l) => l.string).join(" / ")}`
            + ` | 主按钮=${cells.primary.text.string}${this.pending ? "（命令在飞，重复点击已忽略）" : ""}`);
    }

    /** 升级确认：等级阶梯、单价对比、金币账三行、规则说明、两颗按钮。 */
    private renderUpgrade(view: MallView): void {
        const cells = this.upgrade;
        const shopId = this.subject.upgrade;
        const shop: ShopView | undefined = shopId ? view.shops.find((s) => s.id === shopId) : undefined;
        const cost = shop?.upgradeCost ?? null;
        const next = shop?.nextUnitPrice ?? null;
        if (!shop || cost === null || next === null) {
            console.error(`[m06] 升级确认的数据不再成立（${shopId}），关掉弹窗`);
            this.closeAll();
            return;
        }
        cells.title.string = `升级「${shop.name}」`;
        cells.fromBadge.string = `Lv${this.upgradeFromLevel}`;
        cells.toBadge.string = `Lv${this.upgradeFromLevel + 1}`;
        cells.nowValue.string = `${shop.unitPrice}`;
        cells.nowNote.string = `基础 ${shop.unitPriceBase} + 满铺 ${shop.floorBonus}`;
        cells.nextValue.string = `${next}`;
        // 升级后的基础价 = 新单价 − 同一笔加成（§9.2a：满铺只看 unlocked/prepared，与等级无关）
        cells.nextNote.string = `基础 ${next - shop.floorBonus} + 满铺 ${shop.floorBonus}`;
        cells.delta.string = `每位客人多赚 ${next - shop.unitPrice} 金币`;

        cells.cost.string = `− ${cost} 金币`;
        cells.coins.string = `${view.coins}`;
        cells.balance.string = `${view.coins - cost} 金币`;

        // 规则说明不得省略（R-07）：生效时点 + 历史收益不追溯，数字取该店 revenue 原值
        cells.rule.string = `升级从下一位到店的客人开始生效；\n已经赚到的 ${shop.revenue} 金币不会改变，也不会被重新计算。`;

        const affordable = view.coins >= cost;
        cells.ghost.text.string = "取消";
        cells.primary.text.string = "确认升级";
        // 入口那颗按钮已按金币置灰，所以正常态恒可点；真被拒也照样给三重编码
        cells.primary.opacity.opacity = affordable ? 255 : DISABLED_OPACITY;
        this.paintButton(cells.primary, affordable ? "primary" : "disabled");
        this.paintButton(cells.ghost, "ghost");
        paint(cells.primary.corner, (g) => {
            if (affordable) g.clear();
            else lockBadge(g, CORNER_SIZE, rgb(C.veil), rgb(C.woodLine), rgb(C.woodLine));
        });
        // 角标画没画落到 active 上，驱动器才读得到「形状」这一通道（金币不足时才有锁）
        cells.primary.corner.active = !affordable;

        this.log("upgrade", "升级确认", `${cells.title.string} ${cells.fromBadge.string}→${cells.toBadge.string}`
            + ` 当前=${cells.nowValue.string}（${cells.nowNote.string}）→ 升级后=${cells.nextValue.string}（${cells.nextNote.string}）`
            + ` ${cells.delta.string} | 花费=${cells.cost.string} 现有=${cells.coins.string}`
            + ` 余额=${cells.balance.string} | ${cells.rule.string.replace("\n", "")}`
            + ` | 主按钮=${cells.primary.text.string}（${affordable ? "可升级" : "金币不足"}`
            + `，不透明度 ${cells.primary.opacity.opacity}${this.pending ? "，命令在飞" : ""}）`);
    }

    // ---------- 触点 ----------

    /** 详情弹窗主按钮：金币不足 / 满级 / 未开业三种置灰都不发命令（验收第 5、6、7 条）。 */
    private tapDetailPrimary(): void {
        const view = this.view;
        const shopId = this.subject.detail;
        if (!view || !shopId) return;
        const shop = view.shops.find((s) => s.id === shopId);
        if (!shop) return;
        const cost = shop.upgradeCost;
        if (cost === null) {
            console.log(`[m06] 弹窗点击 详情主按钮：${shopId} 已是最高等级，不发命令`);
            return;
        }
        if (!shop.prepared) {
            console.log(`[m06] 弹窗点击 详情主按钮：${shopId} 还没开业，不发命令（服务端会拒 SHOP_NOT_OPEN）`);
            return;
        }
        if (view.coins < cost) {
            console.log(`[m06] 弹窗点击 详情主按钮：还差 ${cost - view.coins} 金币，不发升级命令`);
            return;
        }
        console.log(`[m06] 弹窗点击 详情主按钮 → 打开升级确认（${shopId}）`);
        this.deps.handlers.onAskUpgrade(shopId);
    }

    /** 解锁确认的「确认解锁」。买不买得起在点击现场重算，不信入口那次判断。 */
    private tapUnlockConfirm(slotId: string): void {
        const view = this.view;
        const cost = view?.nextUnlockCost ?? null;
        if (!view || cost === null) return;
        if (this.pending) {
            console.log(`[m06] 弹窗点击 确认解锁：上一条命令还在飞，忽略重复点击（${slotId}）`);
            return;
        }
        if (view.coins < cost) {
            console.log(`[m06] 弹窗点击 确认解锁：还差 ${cost - view.coins} 金币，不发命令`);
            return;
        }
        this.pending = true;
        this.renderUnlock(view);
        console.log(`[m06] 弹窗点击 确认解锁 → 提交命令（${slotId}，${cost} 金币）`);
        this.deps.handlers.onConfirmUnlock(slotId);
    }

    /** 升级确认的「确认升级」。同一道在飞与买得起的现场复核。 */
    private tapUpgradeConfirm(shopId: string): void {
        const view = this.view;
        const shop = view?.shops.find((s) => s.id === shopId);
        if (!view || !shop) return;
        const cost = shop.upgradeCost;
        if (cost === null) {
            console.log(`[m06] 弹窗点击 确认升级：${shopId} 已是最高等级，不发命令`);
            return;
        }
        if (this.pending) {
            console.log(`[m06] 弹窗点击 确认升级：上一条命令还在飞，忽略重复点击（${shopId}）`);
            return;
        }
        if (!shop.prepared) {
            console.log(`[m06] 弹窗点击 确认升级：${shopId} 还没开业，不发命令`);
            return;
        }
        if (view.coins < cost) {
            console.log(`[m06] 弹窗点击 确认升级：还差 ${cost - view.coins} 金币，不发命令`);
            return;
        }
        this.pending = true;
        this.renderUpgrade(view);
        console.log(`[m06] 弹窗点击 确认升级 → 提交命令（${shopId}，${cost} 金币）`);
        this.deps.handlers.onConfirmUpgrade(shopId);
    }

    // ---------- 建节点 ----------

    /** 面板骨架：木底 + 奶白内容板 + 关闭钮。返回内容区容器（子元素按稿坐标往里摆）。 */
    private buildPanel(name: PopupName, caption: string): {
        panel: Node; inner: Node; content: Node; close: Node;
    } {
        const width = contentWidth(name);
        const innerWidth = PANEL_WIDTH - PANEL_INSET * 2;
        const innerHeight = CONTENT_HEIGHT[name] + (PAD[name] + INNER_BORDER) * 2;
        const panelHeight = innerHeight + PANEL_INSET * 2;
        const panel = container(this.overlay, `Popup${caption}`, (DESIGN_WIDTH - PANEL_WIDTH) / 2, TOP[name],
            PANEL_WIDTH, panelHeight);
        paint(panel, (g) => paintPanelSkin(g, panelHeight));
        // 面板自己吃掉点击：否则点在面板空白处会穿到遮罩上，被当成"点遮罩 = 取消"
        panel.addComponent(BlockInputEvents);

        const inner = container(panel, "Inner", PANEL_INSET, PANEL_INSET, innerWidth, innerHeight);
        paint(inner, (g) => paintInnerSkin(g, innerHeight));
        const content = container(inner, "Content", PAD[name] + INNER_BORDER, PAD[name] + INNER_BORDER,
            width, CONTENT_HEIGHT[name]);
        const close = container(panel, "Close", PANEL_WIDTH - CLOSE_SIZE + CLOSE_OVERHANG, -CLOSE_OVERHANG,
            CLOSE_SIZE, CLOSE_SIZE);
        paint(close, (g) => {
            disc(g, CLOSE_SIZE, rgb(C.woodMid), rgb(C.woodLine), 4);
            const arm = 21;
            strokePath(g, rgb(C.woodPale), 6, [[-arm / 2.6, arm / 2.6], [arm / 2.6, -arm / 2.6]]);
            strokePath(g, rgb(C.woodPale), 6, [[-arm / 2.6, -arm / 2.6], [arm / 2.6, arm / 2.6]]);
        });
        close.on(Node.EventType.TOUCH_END, (e: { propagationStopped: boolean }) => {
            e.propagationStopped = true;
            this.closeTop();
        }, close);
        return { panel, inner, content, close };
    }

    /**
     * 详情弹窗按状态收面板高度。
     * 满级与未开业要隐藏「升级预告」那两行（裁定 D-5），留着原高度就等于在奶白板底下空出
     * 一整块没画完的地方——稿上没这两个态，收高度是"同一套形状往下推"里最不伤观感的一种。
     *
     * ⚠️ 收的是**面板与奶白板**，内容区尺寸不动、只重新按父节点高度摆位：place() 的 y 是
     * 由"父节点高度 − top"算出来的，子元素摆位时父节点高度已经变了，所以必须自顶向下
     * 依次 place(panel) → place(inner) → place(content)，让内容始终贴面板顶边锚定。
     * 早先连 content 一起改尺寸，结果内容整体比板面上缘高出一截，标题压到木框上（实测）。
     */
    private resizeDetail(contentHeight: number): void {
        const cells = this.detail;
        const innerHeight = contentHeight + (PAD.detail + INNER_BORDER) * 2;
        const panelHeight = innerHeight + PANEL_INSET * 2;
        if (Math.abs(cells.panel.getComponent(UITransform)!.height - panelHeight) < 1) return;
        const innerWidth = PANEL_WIDTH - PANEL_INSET * 2;
        paint(cells.panel, (g) => paintPanelSkin(g, panelHeight));
        place(cells.panel, (DESIGN_WIDTH - PANEL_WIDTH) / 2, TOP.detail, PANEL_WIDTH, panelHeight);
        paint(cells.inner, (g) => paintInnerSkin(g, innerHeight));
        place(cells.inner, PANEL_INSET, PANEL_INSET, innerWidth, innerHeight);
        place(cells.content, PAD.detail + INNER_BORDER, PAD.detail + INNER_BORDER,
            contentWidth("detail"), CONTENT_HEIGHT.detail);
        place(cells.close, PANEL_WIDTH - CLOSE_SIZE + CLOSE_OVERHANG, -CLOSE_OVERHANG, CLOSE_SIZE, CLOSE_SIZE);
    }

    private buildDetail(): DetailCells {
        const { panel, inner, content, close } = this.buildPanel("detail", "Detail");
        const width = contentWidth("detail");

        const head = container(content, "Head", 0, 0, width, 48);
        const name = label(head, "Name", "", FONT.title, C.woodLine, 0, 0, 140, 48, true, Label.HorizontalAlign.LEFT);
        const badge = container(head, "LevelBadge", 154, 6, 100, 36);
        paint(badge, (g) => rounded(g, 100, 36, 8, rgb(C.woodBase), rgb(C.woodLine), 3));
        const badgeText = label(badge, "Level", "", FONT.sub, C.woodPale, 0, 0, 100, 36);
        const statusTick = container(head, "Tick", 340, 7, 34, 34);
        paint(statusTick, (g) => {
            disc(g, 34, rgb(C.greenBrand), rgb(C.woodLine), 3);
            strokePath(g, rgb(C.creamBright), 4, [[-8, 0], [-2, -6], [9, 5]]);
        });
        const statusText = label(head, "Status", "营业中", 22, C.greenDeep, 382, 10, width - 382, 28, true,
            Label.HorizontalAlign.LEFT);

        const art = container(content, "Art", 0, 64, width, ART_HEIGHT);
        paint(art, (g) => rounded(g, width, ART_HEIGHT, 10, rgb(C.creamBg), rgb(C.woodLine), INNER_BORDER));
        const artNode = new Node("Shop");
        art.addChild(artNode);
        artNode.layer = art.layer;
        const artSprite = artNode.addComponent(Sprite);
        artSprite.sizeMode = Sprite.SizeMode.CUSTOM;
        artNode.addComponent(UITransform).setContentSize(width - INNER_BORDER * 2, ART_HEIGHT - INNER_BORDER * 2);
        const veilNode = container(art, "Veil", INNER_BORDER, INNER_BORDER,
            width - INNER_BORDER * 2, ART_HEIGHT - INNER_BORDER * 2);
        const veil = veilNode.addComponent(Graphics);

        const price = this.buildKv(content, "Price", "单客收益", 270, width);
        const formulaBox = container(content, "Formula", 0, 324, width, 60);
        paint(formulaBox, (g) => rounded(g, width, 60, 10, rgb(C.creamWarm), rgb(C.woodLine), 3));
        const formula = label(formulaBox, "Text", "", 25, C.woodLine, 0, 12, width, 36);
        this.buildDivider(content, "Divider1", 394, width);
        const visitors = this.buildKv(content, "Visitors", "累计客流", 413, width);
        const revenue = this.buildKv(content, "Revenue", "累计收益", 457, width);

        const preview = container(content, "Preview", 0, 501, width, PREVIEW_BLOCK);
        this.buildDivider(preview, "Divider2", 0, width);
        const previewLevel = this.buildKv(preview, "Level", "升到 Lv3 后", DIVIDER_BLOCK, width);
        const previewCost = this.buildKv(preview, "Cost", "升级花费", DIVIDER_BLOCK + KV_HEIGHT, width);

        const buttons = container(content, "Buttons", 0, DETAIL_BUTTONS_TOP, width, BUTTON_HEIGHT);
        const buttonWidth = (width - BUTTON_GAP) / 2;
        const ghost = this.buildButton(buttons, "Ghost", "再想想", 0, buttonWidth);
        const primary = this.buildButton(buttons, "Primary", "升级到 Lv3", buttonWidth + BUTTON_GAP, buttonWidth);
        this.tap(ghost.node, () => this.closeTop());
        this.tap(primary.node, () => this.tapDetailPrimary());

        return {
            panel, inner, content, close, name, badgeText, statusText, statusTick, art: artSprite, veil,
            price, formula, visitors, revenue, preview, previewLevel, previewCost,
            buttons, primary, ghost,
        };
    }

    private buildUnlock(): UnlockCells {
        const { panel, content } = this.buildPanel("unlock", "Unlock");
        const width = contentWidth("unlock");

        label(content, "Title", "解锁铺位", FONT.title, C.woodLine, 0, 0, width, 48);
        const subtitle = label(content, "Subtitle", "二层 · 2 号铺位「甜屋」", 23, C.woodDark, 0, 56, width, 32, false);

        // 前后对照：同一张空铺底图，左边压暗 + 纱罩 + 锁角标，右边亮着
        const pair = container(content, "Pair", 0, 96, width, PAIR_BOX.height + 36);
        const before = this.buildPairBox(pair, "Before", 0, "S1");
        const arrow = container(pair, "Arrow", PAIR_BOX.width + 6, (PAIR_BOX.height - 22) / 2, 16, 22);
        paint(arrow, (g) => arrowRight(g, 16, 22, rgb(C.woodDark)));
        const after = this.buildPairBox(pair, "After", PAIR_BOX.width + 6 + 16 + 6, "S2");
        label(pair, "BeforeText", "未开放", FONT.sub, C.woodBase, 0, PAIR_BOX.height + 8, PAIR_BOX.width, 28, false);
        label(pair, "AfterText", "可开业", FONT.sub, C.greenDeep,
            PAIR_BOX.width + 28, PAIR_BOX.height + 8, PAIR_BOX.width, 28, false);

        const money = this.buildMoneyBox(content, "Money", 326, width, ["解锁花费", "现有金币", "解锁后余额"]);
        const chg = container(content, "Change", 0, 479, width, 148);
        label(chg, "Title", "解锁后会怎样", 23, C.woodLine, 0, 0, width, 32, true, Label.HorizontalAlign.LEFT);
        const lines = ["", "", ""].map((_, index) => label(chg, `Line${index}`, "", 22, C.woodDark, 0,
            40 + index * 36, width, 34, false, Label.HorizontalAlign.LEFT));

        const buttons = container(content, "Buttons", 0, 647, width, BUTTON_HEIGHT);
        const buttonWidth = (width - BUTTON_GAP) / 2;
        const ghost = this.buildButton(buttons, "Ghost", "再想想", 0, buttonWidth);
        const primary = this.buildButton(buttons, "Primary", "确认解锁", buttonWidth + BUTTON_GAP, buttonWidth);
        const slotId = () => this.subject.unlock ?? "";
        this.tap(ghost.node, () => this.closeTop());
        this.tap(primary.node, () => this.tapUnlockConfirm(slotId()));

        return {
            panel, subtitle, beforeArt: before.art, beforeVeil: before.veil, afterArt: after.art,
            cost: money[0], coins: money[1], balance: money[2], lines, primary, ghost,
        };
    }

    private buildUpgrade(): UpgradeCells {
        const { panel, content } = this.buildPanel("upgrade", "Upgrade");
        const width = contentWidth("upgrade");

        const title = label(content, "Title", "升级「咖啡」", FONT.title, C.woodLine, 0, 0, width, 48);

        const lvline = container(content, "Levels", 0, 62, width, 44);
        const badgeWidth = 84;
        const groupWidth = badgeWidth * 2 + 16 + 28;
        const fromBadge = this.buildLevelBadge(lvline, "From", (width - groupWidth) / 2, 44 - 36, badgeWidth, C.woodBase);
        const arrow = container(lvline, "Arrow", (width - 16) / 2, 11, 16, 22);
        paint(arrow, (g) => arrowRight(g, 16, 22, rgb(C.woodDark)));
        const toBadge = this.buildLevelBadge(lvline, "To", (width + groupWidth) / 2 - badgeWidth, 44 - 36,
            badgeWidth, C.greenBrand);

        const price = container(content, "Price", 0, 124, width, 140);
        const cellWidth = 229;
        const now = this.buildPriceCell(price, "Now", "当前单客", 0, cellWidth, false);
        const cellArrow = container(price, "Arrow", cellWidth + 20, 59, 16, 22);
        paint(cellArrow, (g) => arrowRight(g, 16, 22, rgb(C.woodLine)));
        const next = this.buildPriceCell(price, "Next", "升级后单客", cellWidth + 56, cellWidth, true);
        const delta = label(content, "Delta", "", 26, C.greenDeep, 0, 270, width, 36);

        const money = this.buildMoneyBox(content, "Money", 322, width, ["升级花费", "现有金币", "升级后余额"]);

        const rule = container(content, "Rule", 0, 475, width, 98);
        paint(rule, (g) => {
            g.clear();
            g.fillColor = withAlpha(C.creamWarm, 0.55);
            g.roundRect(-width / 2, -49, width, 98, 10);
            g.fill();
            // 稿上是 3px 虚线描边，Graphics 没有虚线：退成实线并在 §10.3 登记这条按实调整
            g.strokeColor = rgb(C.woodLight);
            g.lineWidth = 3;
            g.roundRect(-width / 2, -49, width, 98, 10);
            g.stroke();
        });
        const ruleText = label(rule, "Text", "", 22, C.woodDark, 14, 13, width - 28, 72, false);

        const buttons = container(content, "Buttons", 0, 593, width, BUTTON_HEIGHT);
        const buttonWidth = (width - BUTTON_GAP) / 2;
        const ghost = this.buildButton(buttons, "Ghost", "取消", 0, buttonWidth);
        const primary = this.buildButton(buttons, "Primary", "确认升级", buttonWidth + BUTTON_GAP, buttonWidth);
        const shopId = () => this.subject.upgrade ?? "";
        this.tap(ghost.node, () => this.closeTop());
        this.tap(primary.node, () => this.tapUpgradeConfirm(shopId()));

        return {
            panel, title, fromBadge, toBadge, nowValue: now.value, nowNote: now.note,
            nextValue: next.value, nextNote: next.note, delta,
            cost: money[0], coins: money[1], balance: money[2], rule: ruleText, primary, ghost,
        };
    }

    // ---------- 小构建块 ----------

    /** 一左一右一行的数据（稿 .kv：正文 26 左、数值 28 右，行高 44）。 */
    private buildKv(parent: Node, name: string, caption: string, top: number, width: number): KvRow {
        const row = container(parent, `Kv${name}`, 0, top, width, KV_HEIGHT);
        const key = label(row, "Key", caption, FONT.body, C.woodDark, 0, 9, width / 2, 26, true,
            Label.HorizontalAlign.LEFT);
        const value = label(row, "Value", "", FONT.row, C.woodLine, width / 2, 8, width / 2, 28, true,
            Label.HorizontalAlign.RIGHT);
        return { key, value };
    }

    /** 分隔线（稿 .hr：3 高、woodLine @16%）。 */
    private buildDivider(parent: Node, name: string, top: number, width: number): void {
        const line = container(parent, name, 0, top, width, 3);
        paint(line, (g) => rect(g, width, 3, withAlpha(C.woodLine, 0.16)));
    }

    /**
     * 金币账那一块（稿 .money：底 creamWarm + 三行 + 上下 14 内边距 + 描边 3）。
     * 返回三行的**数值** Label，颜色在渲染时按稿分工改（花费金色、余额绿色）。
     */
    private buildMoneyBox(parent: Node, name: string, top: number, width: number, captions: string[]): Label[] {
        const box = container(parent, name, 0, top, width, MONEY_HEIGHT);
        paint(box, (g) => rounded(g, width, MONEY_HEIGHT, 10, rgb(C.creamWarm), rgb(C.woodLine), 3));
        const rowWidth = width - 32;
        return captions.map((caption, index) => {
            const row = container(box, `Row${index}`, 16, 14 + index * MONEY_ROW, rowWidth, MONEY_ROW);
            label(row, "Key", caption, FONT.body, C.woodDark, 0, 5, 240, 25, true, Label.HorizontalAlign.LEFT);
            return label(row, "Value", "", FONT.row, C.woodLine, rowWidth - 240, 3, 240, 29, true,
                Label.HorizontalAlign.RIGHT);
        });
    }

    /** 前后对照的一格：底 + 空铺底图 + 纱罩 + 锁角标（锁只在未开放那格画）。 */
    private buildPairBox(parent: Node, name: string, left: number, state: SlotState): {
        art: Sprite; veil: Graphics;
    } {
        const box = container(parent, name, left, 0, PAIR_BOX.width, PAIR_BOX.height);
        paint(box, (g) => rounded(g, PAIR_BOX.width, PAIR_BOX.height, 10, rgb(C.creamBg), rgb(C.woodLine), INNER_BORDER));
        const artNode = new Node("Art");
        box.addChild(artNode);
        artNode.layer = box.layer;
        const sprite = artNode.addComponent(Sprite);
        sprite.sizeMode = Sprite.SizeMode.CUSTOM;
        artNode.addComponent(UITransform).setContentSize(PAIR_BOX.width - 8, PAIR_BOX.height - 8);
        const veilNode = container(box, "Veil", INNER_BORDER, INNER_BORDER,
            PAIR_BOX.width - INNER_BORDER * 2, PAIR_BOX.height - INNER_BORDER * 2);
        const veil = veilNode.addComponent(Graphics);
        paintVeil(veil, PAIR_BOX.width - INNER_BORDER * 2, PAIR_BOX.height - INNER_BORDER * 2, state);
        if (state === "S1") {
            const lock = container(box, "Lock", PAIR_BOX.width - 54, 10, 44, 44);
            paint(lock, (g) => lockChip(g, 44, rgb(C.veil), rgb(C.woodLine), rgb(C.woodLine)));
        }
        return { art: sprite, veil };
    }

    /** 等级徽章（稿 06 的 .lvbadge：木底 / 升级后那枚用品牌绿）。 */
    private buildLevelBadge(parent: Node, name: string, left: number, top: number, width: number, bg: string): Label {
        const badge = container(parent, name, left, top, width, 36);
        paint(badge, (g) => rounded(g, width, 36, 8, rgb(bg), rgb(C.woodLine), 3));
        return label(badge, "Text", "Lv1", 24, C.woodPale, 0, 0, width, 36);
    }

    /** 单价对比的一格（稿 06 的 .pcell：升级后那块用橱窗暖光底，复用"灯亮起来"的既有语言）。 */
    private buildPriceCell(parent: Node, name: string, caption: string, left: number, width: number, lit: boolean): {
        value: Label; note: Label;
    } {
        const cell = container(parent, name, left, 0, width, 140);
        paint(cell, (g) => {
            g.clear();
            if (lit) {
                // 稿上是 glow-core → glow-window 的渐变，这里退成橱窗暖光单色
                rounded(g, width, 140, 12, rgb(C.glowWindow), rgb(C.woodLine), 3);
            } else {
                rounded(g, width, 140, 12, rgb(C.creamWarm), rgb(C.woodLine), 3);
            }
        });
        label(cell, "Key", caption, FONT.sub, C.woodDark, 0, 14, width, 28, false);
        const value = label(cell, "Value", "", FONT.stat + 8, C.woodLine, 0, 40, width, 54);
        const note = label(cell, "Note", "", 20, C.woodLine, 0, 96, width, 26, false);
        return { value, note };
    }

    /** 一颗按钮：底由渲染重画，文字带描边（稿的 text-shadow 四向）。 */
    private buildButton(parent: Node, name: string, caption: string, left: number, width: number): PopupButton {
        const node = container(parent, name, left, 0, width, BUTTON_HEIGHT);
        const graphics = node.addComponent(Graphics);
        const opacity = node.addComponent(UIOpacity);
        const text = label(node, "Text", caption, FONT.button, C.creamBright, 0, 14, width, 36);
        text.enableOutline = true;
        text.outlineColor = rgb(C.woodLine);
        text.outlineWidth = 2;
        const corner = container(node, "Corner", width - CORNER_SIZE + 13, -18, CORNER_SIZE, CORNER_SIZE);
        corner.addComponent(Graphics);
        // 默认收起：Graphics 空着虽然画不出东西，但节点 active 会被取证通道读成"有角标"，
        // 而形状通道正是禁用态三重编码要验的那一列（渲染时按状态显式开合）。
        corner.active = false;
        return { node, graphics, corner, text, opacity, width };
    }

    /** 按钮的触点统一从这里挂，省掉三处重复的事件对象类型断言。 */
    private tap(node: Node, handler: () => void): void {
        node.on(Node.EventType.TOUCH_END, (event: { propagationStopped: boolean }) => {
            // 不吃掉事件就会冒泡到面板 / 遮罩，一次点击同时"确认升级 + 取消弹窗"
            event.propagationStopped = true;
            handler();
        }, node);
    }

    /** 按稿色值重画一颗按钮。⚠️ 一次 clear 画完金晕、底、描边与高光（同节点两个 Graphics 会互擦）。 */
    private paintButton(button: PopupButton, look: ButtonLook): void {
        const g = button.graphics;
        const width = button.width;
        const height = BUTTON_HEIGHT;
        g.clear();
        if (look === "primary") {
            g.strokeColor = withAlpha(C.gold, 0.45);
            g.lineWidth = 5;
            g.roundRect(-width / 2 - 5, -height / 2 - 5, width + 10, height + 10, 18);
            g.stroke();
        }
        const body = look === "primary" ? C.woodMid : C.woodBase;
        const top = look === "primary" ? C.woodLight : C.woodMid;
        g.fillColor = rgb(body);
        g.roundRect(-width / 2, -height / 2, width, height, 14);
        g.fill();
        g.strokeColor = rgb(C.woodLine);
        g.lineWidth = 4;
        g.roundRect(-width / 2, -height / 2, width, height, 14);
        g.stroke();
        g.fillColor = rgb(top);
        g.roundRect(-width / 2 + 6, height / 2 - 10, width - 12, 4, 2);
        g.fill();
    }

    // ---------- 共用判据（与页面层同源，全部读服务端字段） ----------

    private slotOfShop(shopId: string | null): SlotConfig | null {
        if (!shopId) return null;
        return this.deps.config.slots.find((s) => s.shopId === shopId) ?? null;
    }

    private shopName(slot: SlotConfig): string {
        return this.deps.config.shops.find((s) => s.id === slot.shopId)?.name ?? slot.shopId;
    }

    /** 逐层「已开业数 / 铺位数」——GDD §6.1 明文许可的纯计数，不是判定加成。 */
    private floorProgress(view: MallView, floor: number): { opened: number; total: number } {
        const slots = this.deps.config.slots.filter((s) => s.floor === floor);
        const opened = slots.filter((s) => {
            if (view.unlockedSlots.indexOf(s.id) < 0) return false;
            const shop = view.shops.find((v) => v.id === s.shopId);
            return Boolean(shop && shop.prepared);
        }).length;
        return { opened, total: slots.length };
    }

    /** 逐层**已解锁**数（解锁确认的变化清单与布局页标题同口径）。 */
    private unlockedOn(view: MallView, floor: number): number {
        return this.deps.config.slots
            .filter((s) => s.floor === floor && view.unlockedSlots.indexOf(s.id) >= 0)
            .length;
    }

    private fullFloors(view: MallView): number[] {
        const floors = Array.from(new Set(this.deps.config.slots.map((s) => s.floor))).sort((a, b) => a - b);
        return floors.filter((floor) => {
            const { opened, total } = this.floorProgress(view, floor);
            return total > 0 && opened === total;
        });
    }

    private push(name: PopupName): void {
        if (this.stack[this.stack.length - 1] === name) {
            this.sync();
            return;
        }
        this.stack.push(name);
        this.sync();
    }

    /** 只让最上面那层可见：两层同时挂着会把下层文字叠上来，读数也归因不到是哪一屏。 */
    private sync(): void {
        const top = this.stack[this.stack.length - 1] ?? null;
        for (const [name, node] of Object.entries(this.panels)) {
            node.active = name === top;
        }
        this.overlay.active = this.stack.length > 0;
        if (!this.stack.length) this.notice.active = false;
    }

    /**
     * 渲染摘要：把**实际写进 Label 的串**原样拼成一行日志。
     * 与 MallPages 同一个理由——`hud` 读数要能归因到"显示的是哪个数"，按截图观感不算判据。
     * 值没变就不重复打，免得每 5 秒一次 settle 刷屏。
     */
    private log(key: PopupName, popup: string, text: string): void {
        if (text === this.lastLog[key]) return;
        this.lastLog[key] = text;
        console.log(`[m06] 弹窗渲染 ${popup} ${text}`);
    }
}

/** 稿上的「一层 / 二层」短名（与页面层同一套词，楼层在配置里是数字）。 */
const FLOOR_CN = ["", "一层", "二层", "三层", "四层", "五层"];

function floorLabel(floor: number): string {
    return FLOOR_CN[floor] ?? `${floor}层`;
}

/**
 * 木面板底。一次 clear 画完：稿上的三段木色渐变在 Graphics 里退成「底色 + 顶部一条高光」，
 * 不影响判读；真面板图（ui_panel_wood）到位后换件，见 docs/M06 §9.1 的 P-2 乙那条遗留。
 */
function paintPanelSkin(g: Graphics, panelHeight: number): void {
    g.clear();
    rounded(g, PANEL_WIDTH, panelHeight, PANEL_RADIUS, rgb(C.woodBase));
    g.lineWidth = PANEL_BORDER;
    g.strokeColor = rgb(C.woodLine);
    g.roundRect(-PANEL_WIDTH / 2, -panelHeight / 2, PANEL_WIDTH, panelHeight, PANEL_RADIUS);
    g.stroke();
    g.fillColor = rgb(C.woodLight);
    g.roundRect(-PANEL_WIDTH / 2 + 10, panelHeight / 2 - 14, PANEL_WIDTH - 20, 5, 2);
    g.fill();
}

/** 内嵌奶白内容板（木底压正文对比度不足 7:1，正文一律落在这块板上）。 */
function paintInnerSkin(g: Graphics, innerHeight: number): void {
    rounded(g, PANEL_WIDTH - PANEL_INSET * 2, innerHeight, INNER_RADIUS, rgb(C.creamBright), rgb(C.woodLine), INNER_BORDER);
}
