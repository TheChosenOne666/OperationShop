// M06 页面层：经营页（稿 02）与布局页（稿 01）。
//
// 为什么节点由代码建、不写进 Mall.scene：手写 89KB 场景的序列化字段是本项目点名的风险区
// （没有 Cocos 连接器可用），代码建的节点可 diff、可 review。先例：M05 的客人也不落 prefab
// （主理人 2026-09-21 拍板，见 docs/M05-经营闭环.md §2 D3）。切页只切 active，主界面与 HUD
// 都不重建，所以轮询与串行队列不受影响（M06 验收第 9 条）。
//
// 规格来源（逐条对齐，不自行发挥）：
//   design/ui-mockups/02-经营页.html、01-布局页.html（含各自的「设计说明」面板与尺寸要点）
//   《系统-经营与成长》§6.1 字段契约、§6.2「不得自行判定满铺加成」
//   《美术圣经》§4.2 顶部避让 48 / §6.1 HUD 160 + 上方禁放 53 / §9.3 状态三通道编码
//   docs/M06-页面与弹窗.md §3 逐屏需求点、§5 验收标准
//
// 四条实现约束：
//   1. **渲染同步且幂等**：页面随快照每 5 秒重渲染一次，取图只用 MallScene 预加载好的缓存。
//      渲染路径上不许引异步——回调会在多轮之间堆积、顺序不可控（M04/M05 同一结论）。
//   2. **数值只读服务端**：`todayEarned` / `unitPriceBase` / `floorBonus` / `nextUnitPrice` /
//      `config.fullFloorBonus` 全是服务端给的，界面原样显示。客户端只允许两类运算：逐层
//      「已开业数 / 铺位数」的纯计数（GDD §6.1 明文许可），以及不含规则常量的加减
//      （「再攒 N 金币」，边界见 ADR 0001 的纯算术一节）。单价表与解锁价本身不进本文件。
//   3. **页面盖住主界面时必须挡住点击**：铺位节点的 TOUCH_END 在页面之下仍会被命中
//      （Cocos 的命中测试只看带监听器的节点，不看上面盖了不透明图），所以整页挂
//      BlockInputEvents，否则"看不见的铺位"会被点出去开店。
//   4. **一个节点只有一个 Graphics**：Graphics 每次重画都从 clear() 起，同一节点挂第二个
//      会把前一个画的东西抹掉。所以角标这类"底 + 记号"的图一律走一个函数一次画完。
import { BlockInputEvents, Graphics, Label, Node, Sprite, SpriteFrame, UIOpacity, UITransform } from "cc";
import type { Config, MallView, ShopView, SlotConfig } from "./ApiTypes";
import {
    C, CONTENT_WIDTH, DESIGN_WIDTH, FONT, HUD_HEIGHT, MARGIN, SAFE_TOP,
    type SlotState,
    container, disc, label, lockBadge, lockChip, paint, paintVeil, plusBadge, rect, ring, rounded, rgb, strokePath,
} from "./MallTheme";

/** 三页加主界面。好友页属 M08，导航入口在场景的 HUD 上，这里不管它的内容。 */
export type PageName = "home" | "manage" | "layout";

/** 页面上报给场景的动作。页面不碰传输层，只报意图（ADR 0001）。 */
export interface PagesHandlers {
    /** 经营页的金色「开业」按钮。开店不花金币、幂等，所以不做确认弹窗（R-03、GDD §2.4）。 */
    onPrepare(shopId: string): void;
    /** 点营业中的店 → 店铺详情弹窗（M06 任务 6）。 */
    onAskDetail(shopId: string): void;
    /** 布局页主按钮 → 解锁确认弹窗（M06 任务 6）。金币不足时按钮是禁用态，不会走到这里。 */
    onAskUnlock(slotId: string): void;
    /** 未开放铺位按稿指向布局页。 */
    onGotoLayout(): void;
    /** 顶栏返回钮回主界面。 */
    onHome(): void;
}

export interface PagesDeps {
    /** 挂页面的节点（本场景即 Canvas，750×1334）。 */
    root: Node;
    config: Config;
    /** 与 MallScene 共用帧缓存；取不到返回 null，调用侧保持不画而不是画成色块。 */
    frame: (bundleName: string, framePath: string) => SpriteFrame | null;
    handlers: PagesHandlers;
}

/** 稿上画布 1334 高；页面只占 HUD 之上那块，内容区高 1174、中心在 +80。 */
const CANVAS_HEIGHT = 1334;
const CONTENT_HEIGHT = CANVAS_HEIGHT - HUD_HEIGHT;
/** 稿 01 的铺位卡尺寸（@750，两层等宽不做透视缩放）与卡间距。 */
const CARD_F2 = { width: 228, height: 279 };
const CARD_F1 = { width: 347, height: 319 };
const CARD_GAP = 11;
/** 经营页店铺行（稿值 706×146、行距 8）。 */
const ROW_HEIGHT = 146;
const ROW_GAP = 8;

/** 经营页的一行店铺。行数 = 配置的店铺数，构建一次，渲染只改字符串与显隐。 */
interface ManageRow {
    root: Node;
    thumbArt: Sprite;
    veil: Graphics;
    name: Label;
    badge: Node;
    badgeText: Label;
    main: Label;
    sub: Label;
    openedState: Node;
    openButton: Node;
    lockedState: Node;
    shopId: string;
}

/** 布局页的一张铺位卡。 */
interface LayoutCard {
    root: Node;
    art: Sprite;
    veil: Graphics;
    corner: Node;
    name: Label;
    status: Label;
    slot: SlotConfig;
}

export class MallPages {
    private readonly deps: PagesDeps;
    private readonly pages = new Map<PageName, Node>();
    private readonly notices = new Map<PageName, Label>();
    private rows: ManageRow[] = [];
    private cards: LayoutCard[] = [];
    private floors: number[] = [];
    private todayCells: { visitors: Label; earned: Label; bonus: Label } | null = null;
    private layoutCells: { heads: Record<number, Label>; dots: Graphics[]; boost: Label[]; action: Node; actionOpacity: UIOpacity; actionCorner: Node; actionText: Label; actionCost: Label } | null = null;
    /** 最近一次渲染用的快照。按钮点击要判禁用态，只读服务端给的字段，不推算（验收第 2 条）。 */
    private lastView: MallView | null = null;
    /** 上一条渲染摘要，用来去重。 */
    private lastSummary = "";
    private current: PageName = "home";

    constructor(deps: PagesDeps) {
        this.deps = deps;
        this.floors = Array.from(new Set(deps.config.slots.map((s) => s.floor))).sort();
        this.pages.set("manage", this.buildPageRoot("PageManage"));
        this.pages.set("layout", this.buildPageRoot("PageLayout"));
        this.buildManagePage();
        this.buildLayoutPage();
        for (const page of this.pages.values()) page.active = false;
    }

    /** 当前页，供日志与验证核对。 */
    get page(): PageName {
        return this.current;
    }

    /** 切页。同一页重复调用是空操作，免得轮询把用户正在看的页重置。 */
    show(page: PageName): void {
        if (page === this.current) return;
        this.current = page;
        for (const [name, node] of this.pages) node.active = name === page;
        console.log(`[m06] 切页 → ${pageLabel(page)}`);
        // 切页后立刻渲染：空闲 settle 是空转、会被 revision 守卫整份丢掉，等它就等于页面上
        // 一直挂着构建时的占位串，三个状态节点还会同时可见并互相盖住点击（2026-09-22 实测）。
        if (this.lastView) this.render(this.lastView);
    }

    /**
     * 一次幂等重渲染。只在对应页可见时干活：主界面那一屏不显示页面，白渲染浪费帧。
     * 快照是服务端的，本函数只读字段与做纯计数。
     */
    render(view: MallView): void {
        this.lastView = view;
        if (this.pages.get("manage")?.active) this.renderManage(view);
        if (this.pages.get("layout")?.active) this.renderLayout(view);
    }

    /**
     * 页内错误提示。主界面的横幅写在招牌 Label 上，而页面盖在招牌之上看不见它，
     * 所以页面可见时要把同一条信息落到页内（验收第 8 条：失败提示要看得见）。
     */
    notify(text: string | null): void {
        const notice = this.notices.get(this.current);
        if (!notice) return;
        notice.string = text ?? "";
        notice.node.active = Boolean(text);
    }

    dispose(): void {
        for (const page of this.pages.values()) {
            if (page.isValid) page.destroy();
        }
    }

    /**
     * 渲染摘要：把**实际写进每个 Label 的字符串**原样拼成一行日志。
     *
     * 为什么走日志而不是让驱动器去读引擎全局：2026-09-22 实测 web-desktop 产物里没有
     * `window.cc`（全量 grep 零命中），而 `AGENTS.md` 要求界面读数必须能归因到
     * "显示的是哪个数"、不接受按截图观感下判。这行日志记的就是那一刻组件里的串，
     * 比外部猜更硬。值没变就不重复打，免得每 5 秒一行刷屏。
     */
    private summary(page: string, text: string): void {
        const line = `${page} ${text}`;
        if (line === this.lastSummary) return;
        this.lastSummary = line;
        console.log(`[m06] 渲染 ${line}`);
    }

    /** 一行状态的可读形态：三态中文名（与稿上的文字同一套词）。 */
    private static stateText(row: { openedState: Node; openButton: Node; lockedState: Node }): string {
        if (row.openedState.active) return "营业中";
        if (row.openButton.active) return "待开业";
        if (row.lockedState.active) return "未开放";
        return "无状态";
    }

    // ---------- 骨架 ----------

    /** 页面根：内容区大小、奶白底、挡住下层点击。 */
    private buildPageRoot(name: string): Node {
        const root = new Node(name);
        this.deps.root.addChild(root);
        root.layer = this.deps.root.layer;
        root.addComponent(UITransform).setContentSize(DESIGN_WIDTH, CONTENT_HEIGHT);
        // 内容区中心：画布中心 y=0，HUD 占底部 160 ⇒ 内容区中心在 +80
        root.setPosition(0, HUD_HEIGHT / 2, 0);
        root.addComponent(BlockInputEvents);

        const bg = new Node("Bg");
        root.addChild(bg);
        bg.layer = root.layer;
        bg.addComponent(UITransform).setContentSize(DESIGN_WIDTH, CONTENT_HEIGHT);
        paint(bg, (g) => rect(g, DESIGN_WIDTH, CONTENT_HEIGHT, rgb(C.creamBg)));
        return root;
    }

    /** 顶栏：返回钮 + 页名。避让区 48px 内不放任何信息（§4.2）。 */
    private buildTopBar(pageRoot: Node, title: string): void {
        const bar = container(pageRoot, "TopBar", 0, SAFE_TOP, DESIGN_WIDTH, 88);
        const back = container(bar, "Back", MARGIN, 12, 64, 64);
        paint(back, (g) => {
            disc(g, 64, rgb(C.woodMid), rgb(C.woodLine), 3);
            // 返回箭头：朝左的尖角。图标内不放文字字母（规避生图乱码，§4.5 同口径）
            strokePath(g, rgb(C.woodPale), 6, [[9, 15], [-8, 0], [9, -15]]);
        });
        back.on(Node.EventType.TOUCH_END, this.deps.handlers.onHome, back);
        // 标题只留 200 宽：右边到 320 起是页内错误提示位，两者不能叠
        label(bar, "Title", title, FONT.title, C.woodLine, MARGIN + 80, 14, 200, 60, true, Label.HorizontalAlign.LEFT);
    }

    // ---------- 经营页（稿 02） ----------

    private buildManagePage(): void {
        const page = this.pages.get("manage")!;
        this.buildTopBar(page, "经营");

        // 今日概览三格（稿：正好回答"我现在赚多少、为什么"）
        const today = container(page, "Today", MARGIN, 150, CONTENT_WIDTH, 112);
        paint(today, (g) => rounded(g, CONTENT_WIDTH, 112, 12, rgb(C.creamBright), rgb(C.woodLine), 3));
        const cellWidth = CONTENT_WIDTH / 3;
        ["今日客流", "今日收益", "满铺加成"].forEach((caption, index) => {
            label(today, `Cap${index}`, caption, FONT.sub, C.woodDark, index * cellWidth, 16, cellWidth, 26, false);
        });
        const visitors = label(today, "Visitors", "— / —", FONT.stat, C.woodLine, 0, 52, cellWidth, 46);
        const earned = label(today, "Earned", "+0", FONT.stat, C.goldDeep, cellWidth, 52, cellWidth, 46);
        const bonus = label(today, "Bonus", "—", FONT.body, C.woodLine, cellWidth * 2, 56, cellWidth, 40);
        this.todayCells = { visitors, earned, bonus };

        // 排序说明条（稿原文，逐字照抄）
        const order = container(page, "Order", MARGIN, 286, CONTENT_WIDTH, 40);
        const orderBar = container(order, "Bar", 0, 8, 8, 24);
        paint(orderBar, (g) => rounded(g, 8, 24, 4, rgb(C.greenBrand)));
        label(order, "Text", "按客流到店顺序排列（一层临街优先）", FONT.sub, C.woodDark,
            20, 5, CONTENT_WIDTH - 20, 30, false, Label.HorizontalAlign.LEFT);

        // 页内提示位（稿的 336 是「不含提示位」的净布局，这里给提示让出 22px，见 docs/M06 §9.5）
        const notice = label(page, "Notice", "", FONT.sub, C.woodDark, MARGIN, 328, CONTENT_WIDTH, 26, false);
        notice.node.active = false;
        this.notices.set("manage", notice);

        // 列表顺序 = 服务端 shops 数组顺序（一层优先，§7.2 冲突 A 已解决），客户端不自己排
        const list = container(page, "List", MARGIN, 358, CONTENT_WIDTH,
            this.deps.config.shops.length * (ROW_HEIGHT + ROW_GAP) - ROW_GAP);
        this.rows = this.deps.config.shops.map((shop, index) => this.buildManageRow(list, shop.id, index));
    }

    private buildManageRow(parent: Node, shopId: string, index: number): ManageRow {
        const handlers = this.deps.handlers;
        const root = container(parent, `Row${index}`, 0, index * (ROW_HEIGHT + ROW_GAP), CONTENT_WIDTH, ROW_HEIGHT);
        paint(root, (g) => rounded(g, CONTENT_WIDTH, ROW_HEIGHT, 12, rgb(C.creamBright), rgb(C.woodLine), 3));

        const thumb = container(root, "Thumb", 16, 21, 104, 104);
        const art = new Node("Art");
        thumb.addChild(art);
        art.layer = thumb.layer;
        const artSprite = art.addComponent(Sprite);
        artSprite.sizeMode = Sprite.SizeMode.CUSTOM;
        art.getComponent(UITransform)!.setContentSize(104, 104);
        // 纱罩必须是 art 的**后一个兄弟**：父节点自己的渲染永远在子节点之下，挂在父上会被立绘盖掉
        const veilNode = container(thumb, "Veil", 0, 0, 104, 104);
        const veil = veilNode.addComponent(Graphics);

        const name = label(root, "Name", "", FONT.row, C.woodLine, 148, 22, 250, 36, true, Label.HorizontalAlign.LEFT);
        const badge = container(root, "LevelBadge", 402, 24, 62, 32);
        paint(badge, (g) => rounded(g, 62, 32, 6, rgb(C.woodBase), rgb(C.woodLine), 2));
        const badgeText = label(badge, "Level", "", 19, C.woodPale, 0, 0, 62, 32);

        // 行内主文案按稿取 22px（.sub），可用宽度 440：再宽就压到右侧状态区
        const main = label(root, "Main", "", 22, C.woodDark, 148, 64, 440, 32, false, Label.HorizontalAlign.LEFT);
        const sub = label(root, "Sub", "", 20, C.woodDark, 148, 98, 440, 28, false, Label.HorizontalAlign.LEFT);

        // 右侧三态：营业中（绿色对勾 + 文字）/ 待开业（开业按钮 + 加号角标）/ 未开放（锁牌 + 文字）
        const openedState = container(root, "Opened", CONTENT_WIDTH - 130, 30, 110, 96);
        const tick = container(openedState, "Tick", 32, 0, 46, 46);
        paint(tick, (g) => {
            disc(g, 46, rgb(C.greenBrand), rgb(C.woodLine), 3);
            strokePath(g, rgb(C.creamBright), 5, [[-11, 0], [-3, -9], [12, 7]]);
        });
        label(openedState, "Text", "营业中", FONT.sub, C.greenDeep, 0, 56, 110, 28);

        const openButton = container(root, "OpenButton", CONTENT_WIDTH - 148, 33, 132, 80);
        paint(openButton, (g) => {
            rounded(g, 132, 80, 12, rgb(C.woodMid), rgb(C.woodLine), 3);
        });
        label(openButton, "Text", "开业", 27, C.creamBright, 0, 22, 132, 36);
        const buttonPlus = container(openButton, "Plus", 105, -12, 42, 42);
        paint(buttonPlus, (g) => plusBadge(g, 42, rgb(C.gold), rgb(C.woodLine), rgb(C.woodLine)));

        const lockedState = container(root, "Locked", CONTENT_WIDTH - 130, 30, 110, 96);
        const chip = container(lockedState, "Chip", 32, 0, 46, 46);
        paint(chip, (g) => lockChip(g, 46, rgb(C.veil), rgb(C.woodLine), rgb(C.woodLine)));
        label(lockedState, "Text", "未开放", FONT.sub, C.woodDark, 0, 56, 110, 28);

        // 触点：整行按当前状态分流，按钮自己吃掉事件——否则点「开业」会同时冒泡成"打开详情"
        root.on(Node.EventType.TOUCH_END, () => {
            const state = this.stateOf(shopId, this.lastView);
            if (state === "S3") handlers.onAskDetail(shopId);
            else if (state === "S1") handlers.onGotoLayout();
            else console.log(`[m06] 点了「${shopId}」这一行，待开业行的操作在「开业」按钮上`);
        }, root);
        openButton.on(Node.EventType.TOUCH_END, (event: { propagationStopped: boolean }) => {
            event.propagationStopped = true;
            handlers.onPrepare(shopId);
        }, openButton);
        chip.on(Node.EventType.TOUCH_END, (event: { propagationStopped: boolean }) => {
            event.propagationStopped = true;
            handlers.onGotoLayout();
        }, chip);

        return {
            root, thumbArt: artSprite, veil, name, badge, badgeText, main, sub,
            openedState, openButton, lockedState, shopId,
        };
    }

    private renderManage(view: MallView): void {
        const cells = this.todayCells;
        if (!cells) return;
        // 未冻结时整槽 — / —、不显示分子（GDD §6.1 + 架构现状 §7.2 裁定 L）
        cells.visitors.string = view.dailyVisitorCap === null
            ? "— / —"
            : `${view.visitorsServed}/${view.dailyVisitorCap}`;
        cells.earned.string = `+${view.todayEarned}`;
        const full = this.fullFloors(view);
        cells.bonus.string = full.length
            ? `${full.map(floorLabel).join("、")}生效`
            : "尚未生效";

        for (const row of this.rows) {
            const slot = this.slotOfShop(row.shopId);
            const state = this.stateOf(row.shopId, view);
            const data: ShopView | undefined = view.shops.find((s) => s.id === row.shopId);

            row.openedState.active = state === "S3";
            row.openButton.active = state === "S2";
            row.lockedState.active = state === "S1";
            row.thumbArt.spriteFrame = this.deps.frame("mall_art",
                state === "S1" ? `slt/slt_f${slot?.floor ?? 1}_empty/spriteFrame` : `shop/shop_${row.shopId}_open/spriteFrame`);
            paintVeil(row.veil, 104, 104, state);

            if (state === "S1" || !data) {
                row.name.string = slot ? this.shopName(slot) : row.shopId;
                row.badge.active = false;
                // 稿上这一行有两套措辞（「需先在布局页解锁铺位（800 金币）」与「需先解锁二层 2 号铺位」），
                // 取带价格的那套并去掉「铺位」二字，才能在不压到右侧状态区的前提下放得下（见 docs/M06 §3.1）
                row.main.string = `未开放 · 需先在布局页解锁（${slot?.unlockCost ?? 0} 金币）`;
                row.sub.string = "";
                continue;
            }
            row.name.string = data.name;
            row.badge.active = true;
            row.badgeText.string = `Lv${data.level}`;
            // 单客收益写明「基础 + 满铺」的构成，两半都取服务端字段（不写玩家看不懂数字为什么变）
            const split = data.floorBonus > 0 ? `（${data.unitPriceBase}+满铺${data.floorBonus}）` : "";
            row.main.string = `单客 ${data.unitPrice} 金币${split}`;
            row.sub.string = data.prepared
                ? `累计 ${data.visitors} 客流 · ${data.revenue} 金币`
                : "尚未开业 · 累计 0";
        }

        this.summary("经营页",
            `客流=${cells.visitors.string} 收益=${cells.earned.string} 加成=${cells.bonus.string} | ${this.rows
                .map((row) => `${row.name.string}${row.badge.active ? ` ${row.badgeText.string}` : ""} ${row.main.string}`
                    + `${row.sub.string ? ` ${row.sub.string}` : ""} [${MallPages.stateText(row)}]`)
                .join(" ; ")}`);
    }

    // ---------- 布局页（稿 01） ----------

    private buildLayoutPage(): void {
        const page = this.pages.get("layout")!;
        this.buildTopBar(page, "布局");

        // 楼层自上而下：二层在顶（稿 01 就是这个顺序），同层卡片按 unlockOrder 从左到右
        const heads: Record<number, Label> = {};
        let top = 148;
        for (const floor of [...this.floors].reverse()) {
            const slots = this.deps.config.slots.filter((s) => s.floor === floor).sort(byUnlockOrder);
            const card = floor >= 2 ? CARD_F2 : CARD_F1;
            const head = container(page, `Head${floor}`, MARGIN, top, CONTENT_WIDTH, 46);
            const headBar = container(head, "Bar", 0, 9, 8, 28);
            paint(headBar, (g) => rounded(g, 8, 28, 4, rgb(C.greenBrand)));
            label(head, "Title", `${floorLabel(floor)} · ${floor >= 2 ? "楼面" : "临街"}`, FONT.body, C.woodLine,
                20, 7, 320, 32, true, Label.HorizontalAlign.LEFT);
            heads[floor] = label(head, "Count", "", FONT.body, C.woodLine, CONTENT_WIDTH - 220, 7, 220, 32,
                false, Label.HorizontalAlign.RIGHT);

            const row = container(page, `Slots${floor}`, MARGIN, top + 50, CONTENT_WIDTH, card.height);
            for (const [index, slot] of slots.entries()) {
                this.cards.push(this.buildLayoutCard(row, slot, index * (card.width + CARD_GAP), card));
            }
            // 层间距按稿：上一层卡片下缘到下一层分组标题是 9px（486 = 198+279+9）
            top += 50 + card.height + 9;
        }

        // 同层满铺加成卡：逐层一行，实心点=已生效、空心点=还差几个。
        // 高度收到 132、两行行距 40：卡片下缘必须停在主按钮（1009）之上，
        // 早先按 150 高算会把第二行「二层 0/3」整条压到按钮底下（2026-09-22 截图实测）。
        const boost = container(page, "Boost", MARGIN, top + 4, CONTENT_WIDTH, 132);
        paint(boost, (g) => rounded(g, CONTENT_WIDTH, 132, 10, rgb(C.creamBright), rgb(C.woodLine), 3));
        label(boost, "Title", "同层满铺加成", FONT.body, C.woodLine, 18, 12, 300, 30, true, Label.HorizontalAlign.LEFT);
        const dots: Graphics[] = [];
        const lines: Label[] = [];
        for (const [index] of this.floors.entries()) {
            const dot = container(boost, `Dot${index}`, 20, 60 + index * 40, 14, 14);
            dots.push(dot.addComponent(Graphics));
            lines.push(label(boost, `Line${index}`, "", 23, C.woodDark, 44, 52 + index * 40,
                CONTENT_WIDTH - 62, 34, false, Label.HorizontalAlign.LEFT));
        }

        // 主按钮：压在禁放区上沿（稿 bottom:213 = HUD 160 + 禁放 53）
        const actionTop = CANVAS_HEIGHT - 213 - 112;
        const action = container(page, "Action", MARGIN, actionTop, CONTENT_WIDTH, 112);
        const actionOpacity = action.addComponent(UIOpacity);
        const actionText = label(action, "Text", "解锁铺位", FONT.button, C.creamBright, 40, 38, CONTENT_WIDTH - 260, 36);
        const actionCost = label(action, "Cost", "", FONT.body, C.goldLight, CONTENT_WIDTH - 220, 40, 190, 32);
        const actionCorner = container(action, "Corner", CONTENT_WIDTH - 62, -10, 56, 56);
        action.on(Node.EventType.TOUCH_END, () => this.tapAction(), action);
        this.layoutCells = { heads, dots, boost: lines, action, actionOpacity, actionCorner, actionText, actionCost };

        const notice = label(page, "Notice", "", FONT.sub, C.woodDark, 320, 60, CONTENT_WIDTH - 320 + MARGIN, 26, false);
        notice.node.active = false;
        this.notices.set("layout", notice);
    }

    private buildLayoutCard(parent: Node, slot: SlotConfig, left: number, size: { width: number; height: number }): LayoutCard {
        const handlers = this.deps.handlers;
        const root = container(parent, `Card-${slot.id}`, left, 0, size.width, size.height);
        paint(root, (g) => rounded(g, size.width, size.height, 10, rgb(C.creamBg), rgb(C.woodLine), 3));

        // 立绘区在卡片上半，下半是店名与状态两行文字（稿：art 190 / meta 89）
        const artHeight = size.height - 89;
        const art = new Node("Art");
        root.addChild(art);
        art.layer = root.layer;
        const sprite = art.addComponent(Sprite);
        sprite.sizeMode = Sprite.SizeMode.CUSTOM;
        art.getComponent(UITransform)!.setContentSize(size.width - 6, artHeight);
        art.setPosition(0, size.height / 2 - artHeight / 2 - 3, 0);
        const veilNode = container(root, "Veil", 3, 3, size.width - 6, artHeight);
        veilNode.setPosition(0, size.height / 2 - artHeight / 2 - 3, 0);
        const veil = veilNode.addComponent(Graphics);

        const corner = container(root, "Corner", size.width - 58, 10, 44, 44);
        corner.addComponent(Graphics);

        const name = label(root, "Name", this.shopName(slot), FONT.body, C.woodLine, 12, artHeight + 8,
            size.width - 24, 32, true, Label.HorizontalAlign.LEFT);
        const status = label(root, "Status", "", 21, C.woodDark, 12, artHeight + 44, size.width - 24, 28, false,
            Label.HorizontalAlign.LEFT);

        root.on(Node.EventType.TOUCH_END, () => {
            const state = this.stateOf(slot.shopId, this.lastView);
            if (state === "S1") {
                console.log(`[m06] 点了未开放铺位 ${slot.id}，解锁入口在本页主按钮`);
                return;
            }
            handlers.onAskDetail(slot.shopId);
        }, root);
        return { root, art: sprite, veil, corner, name, status, slot };
    }

    private renderLayout(view: MallView): void {
        const cells = this.layoutCells;
        if (!cells) return;

        for (const card of this.cards) {
            const state = this.stateOf(card.slot.shopId, view);
            const data: ShopView | undefined = view.shops.find((s) => s.id === card.slot.shopId);
            const artUi = card.art.node.getComponent(UITransform)!;
            card.art.spriteFrame = this.deps.frame("mall_art",
                state === "S3" ? `shop/shop_${card.slot.shopId}_open/spriteFrame` : `slt/slt_f${card.slot.floor}_empty/spriteFrame`);
            artUi.setContentSize(card.root.getComponent(UITransform)!.width - 6, artUi.height);
            paintVeil(card.veil, artUi.width, artUi.height, state);
            paint(card.corner, (g) => {
                if (state === "S2") plusBadge(g, 44, rgb(C.woodPale), rgb(C.woodLine), rgb(C.woodLine));
                else if (state === "S1") lockBadge(g, 44, rgb(C.woodPale), rgb(C.woodLine), rgb(C.woodLine));
                else g.clear();
            });
            // 等级只在营业中出现（稿 01：一层两卡写 Lv，二层三卡不写）
            card.name.string = state === "S3" && data ? `${data.name}　Lv${data.level}` : this.shopName(card.slot);
            if (state === "S3" && data) {
                card.status.string = `营业中 · 单客 ${data.unitPrice} 金币`;
                card.status.color = rgb(C.woodBase);
            } else if (state === "S2") {
                card.status.string = "待开业";
                card.status.color = rgb(C.greenDeep);
            } else {
                card.status.string = `未开放 · ${card.slot.unlockCost} 金币`;
                card.status.color = rgb(C.woodDark);
            }
        }

        this.floors.forEach((floor, index) => {
            const { opened, total } = this.floorProgress(view, floor);
            const head = cells.heads[floor];
            // 标题那一格是「已开放」= 已解锁的铺位数（§3.2 的口径写的是 unlockedSlots），
            // 不是已营业数。2026-09-22 用解锁实测撞出来：解开一层 1 个铺位但还没开业时，
            // 按 opened 数会一直显示 0 / 3，玩家刚花掉的 600 金币在界面上等于没发生。
            if (head) head.string = `${this.unlockedOn(view, floor)} / ${total} 已开放`;
            const line = cells.boost[index];
            const dot = cells.dots[index];
            if (!line || !dot) return;
            // 加成金额取服务端配置的显示值，本文件不出现那个数本身；
            // 「是否已生效」同样由服务端回答——该层任一店的 floorBonus > 0 即满铺
            //（ADR 0001 纯算术一节：判满铺是禁止项，SC-M06-QA-001 P2-4）
            const bonus = this.deps.config.fullFloorBonus;
            const fullFloor = view.shops.some((shop) => shop.floor === floor && shop.floorBonus > 0);
            paint(dot.node, (g) => {
                if (fullFloor) disc(g, 14, rgb(C.gold));
                else ring(g, 14, rgb(C.woodDark));
            });
            line.string = fullFloor
                ? `${floorLabel(floor)} ${opened} / ${total} 已满铺 —— 该层单客收益 +${bonus}（已生效）`
                : `${floorLabel(floor)} ${opened} / ${total} —— 再开放 ${total - opened} 个铺位即可获得 +${bonus}`;
        });

        // 主按钮三态：可解锁（金晕 + 加号）/ 金币不足（暗 + 锁 + 写出差多少）/ 全部已解锁
        const slot = this.nextSlot(view);
        const cost = view.nextUnlockCost;
        const affordable = slot !== null && cost !== null && view.coins >= cost;
        // 全部铺位已解锁是**终态**不是"被挡住"：不给锁角标、不压暗（锁=被挡住，与"已全部开放"语义相反；
        // 稿没画这一态，按同一套编码往下推，登记于 docs/M06 §10.3 按实调整）
        const done = slot === null;
        if (slot && cost !== null && !affordable) {
            // 稿：禁用态不写「金币不足」而写差多少；禁止变红（红=危险，违反无失败态）
            cells.actionText.string = `再攒 ${cost - view.coins} 金币`;
            cells.actionCost.string = `${cost} 金币`;
        } else if (slot && cost !== null) {
            cells.actionText.string = `解锁「${this.shopName(slot)}」铺位`;
            cells.actionCost.string = `${cost} 金币`;
        } else {
            cells.actionText.string = "全部铺位已解锁";
            cells.actionCost.string = "";
        }
        paint(cells.actionCorner, (g) => {
            if (affordable) plusBadge(g, 56, rgb(C.gold), rgb(C.woodLine), rgb(C.woodLine));
            else if (done) g.clear();
            else lockBadge(g, 56, rgb(C.veil), rgb(C.woodLine), rgb(C.woodLine));
        });
        paint(cells.action, (g) => {
            if (affordable) {
                // 稿：可解锁带金色呼吸光晕（1.6s 呼吸属表现层，M06 先做静态金晕）
                ring(g, CONTENT_WIDTH + 10, rgb(C.gold), 16, 5, CONTENT_WIDTH + 10, 122);
                rounded(g, CONTENT_WIDTH, 112, 14, rgb(C.woodMid), rgb(C.woodLine), 4);
            } else if (done) {
                rounded(g, CONTENT_WIDTH, 112, 14, rgb(C.woodMid), rgb(C.woodLine), 4);
            } else {
                rounded(g, CONTENT_WIDTH, 112, 14, rgb(C.woodDark), rgb(C.woodLine), 4);
            }
        });
        // 稿：禁用态不透明度 78%（仍 ≥3:1 可见），不是"灰到看不见"；终态满不透明度
        cells.actionOpacity.opacity = affordable || done ? 255 : Math.round(255 * 0.78);
        this.summary("布局页",
            `进度=${this.floors.map((floor) => `${floorLabel(floor)}:${cells.heads[floor]?.string ?? "?"}`).join(" ")} | `
            + `加成=${cells.boost.map((line) => line.string).join(" / ")} | `
            + `卡=${this.cards.map((card) => `${card.slot.id}[${card.name.string}|${card.status.string}]`).join(" ")} | `
            + `主按钮=${cells.actionText.string} ${cells.actionCost.string}`
            + `（${affordable ? "可解锁" : slot ? "禁用" : "无铺位可解锁"}，不透明度 ${cells.actionOpacity.opacity}）`);
    }

    /** 主按钮点击：禁用态不发命令（R-02 要求"金币不足时不可解锁并给出原因"，原因写在按钮上）。 */
    private tapAction(): void {
        const view = this.lastView;
        const slot = view ? this.nextSlot(view) : null;
        const cost = view?.nextUnlockCost ?? null;
        if (!view || !slot || cost === null) {
            // 文案以「主按钮」开头：驱动器的点击送达检查按 [m06] 后的关键词匹配，
            // 写成「布局页主按钮被点」会被判成"事件没送到"的假阴性（2026-09-23 实测）
            console.log("[m06] 主按钮被点，但没有可解锁铺位（已满铺），忽略");
            return;
        }
        if (view.coins < cost) {
            console.log(`[m06] 主按钮处于禁用态（还差 ${cost - view.coins} 金币），不发解锁命令`);
            return;
        }
        this.deps.handlers.onAskUnlock(slot.id);
    }

    // ---------- 共用的判据 ----------

    private slotOfShop(shopId: string): SlotConfig | null {
        return this.deps.config.slots.find((s) => s.shopId === shopId) ?? null;
    }

    private shopName(slot: SlotConfig): string {
        return this.deps.config.shops.find((s) => s.id === slot.shopId)?.name ?? slot.shopId;
    }

    /** 三态判据：不含该铺 ⇒ S1；含而 prepared=false ⇒ S2；prepared=true ⇒ S3。 */
    private stateOf(shopId: string, view: MallView | null): SlotState {
        const slot = this.slotOfShop(shopId);
        if (!slot || !view || view.unlockedSlots.indexOf(slot.id) < 0) return "S1";
        const shop = view.shops.find((s) => s.id === shopId);
        return shop && shop.prepared ? "S3" : "S2";
    }

    /** 下一可解锁铺位：取服务端给的那个，不在客户端按 unlockOrder 猜。 */
    private nextSlot(view: MallView): SlotConfig | null {
        if (!view.nextSlotId) return null;
        return this.deps.config.slots.find((s) => s.id === view.nextSlotId) ?? null;
    }

    /**
     * 逐层「已开业数 / 铺位数」。
     * 这是 GDD §6.1 明文许可的**纯计数**，不是判定加成——加成金额只从服务端字段读。
     */
    private floorProgress(view: MallView, floor: number): { opened: number; total: number } {
        const slots = this.deps.config.slots.filter((s) => s.floor === floor);
        const opened = slots.filter((s) => {
            if (view.unlockedSlots.indexOf(s.id) < 0) return false;
            const shop = view.shops.find((v) => v.id === s.shopId);
            return Boolean(shop && shop.prepared);
        }).length;
        return { opened, total: slots.length };
    }

    /** 逐层**已解锁**数（楼层标题那格「n / m 已开放」用它；满铺加成另算已营业数）。 */
    private unlockedOn(view: MallView, floor: number): number {
        return this.deps.config.slots
            .filter((s) => s.floor === floor && view.unlockedSlots.indexOf(s.id) >= 0)
            .length;
    }

    private fullFloors(view: MallView): number[] {
        // 满铺与否由服务端的 floorBonus 回答（ADR 0001 纯算术一节把「判满铺是否生效」列为禁止项，
        // 服务端为此给了逐店 floorBonus）。逐层计数只用于显示「n / m」，不用于判定。
        return this.floors.filter((floor) =>
            view.shops.some((shop) => shop.floor === floor && shop.floorBonus > 0));
    }
}

/** 稿上的「一层 / 二层」短名。楼层在配置里是数字，展示层要中文，不能写成「2层」。 */
const FLOOR_CN = ["", "一层", "二层", "三层", "四层", "五层"];

function floorLabel(floor: number): string {
    return FLOOR_CN[floor] ?? `${floor}层`;
}

function pageLabel(page: PageName): string {
    return page === "manage" ? "经营页" : page === "layout" ? "布局页" : "主界面";
}

/** 同一层的铺位按 unlockOrder 从左到右排（稿 01 的卡片顺序）。 */
function byUnlockOrder(a: SlotConfig, b: SlotConfig): number {
    return a.unlockOrder - b.unlockOrder;
}

