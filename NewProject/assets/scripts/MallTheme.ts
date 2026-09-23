// M06 的视觉令牌：色值与字号全部照抄《美术圣经》§2.2 主色板与 §8.4 字号表，
// 逐屏复用同一份，避免同一个色在五屏里写出三种写法。
//
// ⚠️ 这里集中的是**呈现层常量**，不是 ADR 0004 管的「数值与规则」——
// 那一条指的是价格、概率、节拍这类玩法数值（归 `config/`），改它们不碰代码；
// 而色值与像素尺寸属于界面资产，与 M04 写在场景里的坐标同类，不进 config。
//
// 坐标约定：设计稿是 @750×1334 的**左上角原点**绝对坐标，Cocos 的 UI 是
// 父节点中心原点、y 向上。换算集中在 place() 一处，逐屏代码里就不再出现减法。
//
// 下半部的 Graphics 绘图原语同样收在这里：页面层与弹窗层画的是同一套木面板、圆角牌、
// 锁与加号角标，散成两份就会漂移（同一个角标在两个文件里画出两种半径这种事）。
import { Color, Graphics, Label, Node, UITransform } from "cc";

/** 设计稿画布（与场景 Canvas 同值，已在 M04 构建产物验证）。 */
export const DESIGN_WIDTH = 750;
export const DESIGN_HEIGHT = 1334;
/** 顶部避让区 48px 不放任何信息（美术圣经 §4.2，避系统胶囊）。 */
export const SAFE_TOP = 48;
/** 底部 HUD 独占 160px，其上方 53px 是禁放区（§6.1）。 */
export const HUD_HEIGHT = 160;
export const NO_ZONE_HEIGHT = 53;
/** 左右安全边距 22，内容宽 706。 */
export const MARGIN = 22;
export const CONTENT_WIDTH = DESIGN_WIDTH - MARGIN * 2;

/** 主色板（美术圣经 §2.2 实测值，逐字取自设计稿的 CSS 变量）。 */
export const C = {
    greenBrand: "#6E8269",
    greenDeep: "#3C4E3C",
    woodPale: "#E8C88A",
    woodLight: "#C89A6E",
    woodMid: "#A8764E",
    woodBase: "#8F6049",
    woodDark: "#6B4B3A",
    woodLine: "#4A2F1E",
    creamBg: "#E4D8C6",
    creamBright: "#F2E8D8",
    creamWarm: "#D9BFA0",
    glowWindow: "#F2DDAE",
    glowCore: "#FFF0C8",
    sunTint: "#E8A868",
    gold: "#EFB021",
    goldLight: "#F7DC63",
    goldDeep: "#C88A10",
    blossom: "#F2CB9E",
    veil: "#CFC6B8",
} as const;

/** 字号表（§8.4：标题 34 / 正文 26 / 辅助 21，按钮 30、HUD 数值 40）。 */
export const FONT = {
    title: 34,
    body: 26,
    sub: 21,
    button: 30,
    buttonDisabled: 27,
    row: 28,
    stat: 36,
} as const;

/** 触控区下限（美术圣经 §9.1 与稿 01 的规格面板一致）。 */
export const MIN_TOUCH = 88;

/** 十六进制色转 Cocos Color。写成一处，免得每屏自己拆 RGB 拆错。 */
export function rgb(hex: string): Color {
    const value = hex.replace("#", "");
    const num = parseInt(value, 16);
    return new Color((num >> 16) & 255, (num >> 8) & 255, num & 255, value.length === 8 ? num & 255 : 255);
}

/**
 * 把「设计稿左上角坐标」的一个矩形摆到父节点（锚点 0.5、尺寸 = 内容区）下。
 * 传入的是稿上的 left/top/width/height，本函数负责翻成 Cocos 的中心坐标。
 */
export function place(node: Node, left: number, top: number, width: number, height: number): UITransform {
    const ui = node.getComponent(UITransform) ?? node.addComponent(UITransform);
    ui.setContentSize(width, height);
    // 父节点必须是「左上角为 (0,0) 的内容区」，即 place 的父层用 pageRoot/panelRoot 建出来的容器。
    const parent = node.parent?.getComponent(UITransform);
    const pw = parent ? parent.width : DESIGN_WIDTH;
    const ph = parent ? parent.height : DESIGN_HEIGHT;
    node.setPosition(left + width / 2 - pw / 2, ph / 2 - (top + height / 2), 0);
    return ui;
}

/** 建一个带 Label 的子节点。字号/颜色按稿，anchor 居中，不做溢出裁剪（文案长度由需求侧固定）。 */
export function label(
    parent: Node,
    name: string,
    text: string,
    size: number,
    colorHex: string,
    left: number,
    top: number,
    width: number,
    height: number,
    bold = true,
    align: Label.HorizontalAlign = Label.HorizontalAlign.CENTER,
): Label {
    const node = new Node(name);
    parent.addChild(node);
    node.layer = parent.layer;
    place(node, left, top, width, height);
    const lb = node.addComponent(Label);
    lb.string = text;
    lb.fontSize = size;
    lb.lineHeight = Math.round(size * 1.25);
    lb.color = rgb(colorHex);
    lb.isBold = bold;
    lb.horizontalAlign = align;
    lb.verticalAlign = Label.VerticalAlign.CENTER;
    return lb;
}

/** 建一个空容器节点（自己也用 left/top 摆位），用来把一组元素整体平移。 */
export function container(parent: Node, name: string, left: number, top: number, width: number, height: number): Node {
    const node = new Node(name);
    parent.addChild(node);
    node.layer = parent.layer;
    place(node, left, top, width, height);
    return node;
}

// ---------- 画图（一律以节点自身中心为原点；每个节点一个 Graphics，一次 clear 画完） ----------

/** 铺位三态（架构现状 §10）：S1 未开放、S2 待开业、S3 营业中。页面与弹窗共用同一套判据。 */
export type SlotState = "S1" | "S2" | "S3";

/**
 * 重画一个节点的 Graphics。传函数而不是传参数列表，是因为角标这类图要"底 + 记号"
 * 在同一个 clear 之后连画——分两次调用就会互相抹掉（一个节点只能挂一个 Graphics，
 * 第二次 addComponent 出来的那个会从 clear() 起把前一个画的东西整片擦掉）。
 */
export function paint(node: Node, draw: (g: Graphics) => void): void {
    const g = node.getComponent(Graphics) ?? node.addComponent(Graphics);
    draw(g);
}

/**
 * 未开放态的压暗与纱罩。主界面用自发光材质压暗（架构现状 §10），页面与弹窗里是重排的
 * 缩略图，用一层半透明达到同样的「明度」通道，不再多复制一份材质实例。
 * 稿值：纱罩 #CFC6B8 @35% + brightness .72（即再压一层黑 @28%）。
 */
export function paintVeil(g: Graphics, width: number, height: number, state: SlotState): void {
    g.clear();
    if (state !== "S1") return;
    rect(g, width, height, withAlpha(C.veil, 0.35), 10);
    rect(g, width, height, new Color(0, 0, 0, 71), 10);
}

export function rounded(g: Graphics, width: number, height: number, radius: number, fill: Color, stroke?: Color, lineWidth = 3): void {
    g.clear();
    g.fillColor = fill;
    g.roundRect(-width / 2, -height / 2, width, height, radius);
    g.fill();
    if (stroke) {
        g.strokeColor = stroke;
        g.lineWidth = lineWidth;
        g.roundRect(-width / 2, -height / 2, width, height, radius);
        g.stroke();
    }
}

export function rect(g: Graphics, width: number, height: number, fill: Color, radius = 0): void {
    g.fillColor = fill;
    if (radius > 0) g.roundRect(-width / 2, -height / 2, width, height, radius);
    else g.rect(-width / 2, -height / 2, width, height);
    g.fill();
}

export function disc(g: Graphics, diameter: number, fill: Color, stroke?: Color, lineWidth = 3): void {
    g.fillColor = fill;
    g.circle(0, 0, diameter / 2);
    g.fill();
    if (stroke) ring(g, diameter, stroke, 0, lineWidth);
}

/** 空心圆或圆角描边框：radius>0 时画成矩形描边（主按钮的金晕、弹窗主按钮的金色光晕用）。 */
export function ring(
    g: Graphics,
    diameter: number,
    stroke: Color,
    radius = 0,
    lineWidth = 3,
    width = diameter,
    height = diameter,
): void {
    g.strokeColor = stroke;
    g.lineWidth = lineWidth;
    if (radius > 0) {
        g.roundRect(-width / 2, -height / 2, width, height, radius);
    } else {
        g.circle(0, 0, diameter / 2);
    }
    g.stroke();
}

/** 折线描边（返回箭头、对勾、关闭钮的两道杠）。 */
export function strokePath(g: Graphics, color: Color, lineWidth: number, points: Array<[number, number]>): void {
    g.strokeColor = color;
    g.lineWidth = lineWidth;
    points.forEach(([x, y], index) => {
        if (index === 0) g.moveTo(x, y);
        else g.lineTo(x, y);
    });
    g.stroke();
}

/** 加号角标（待开业 / 可解锁 / 可升级）。 */
export function plusBadge(g: Graphics, diameter: number, bg: Color, ringColor: Color, mark: Color): void {
    g.clear();
    disc(g, diameter, bg, ringColor, 3);
    const arm = Math.round(diameter * 0.5);
    const bar = Math.max(6, Math.round(diameter / 7));
    g.fillColor = mark;
    g.roundRect(-bar / 2, -arm / 2, bar, arm, 3);
    g.roundRect(-arm / 2, -bar / 2, arm, bar, 3);
    g.fill();
}

/** 锁形角标（未开放 / 禁用态）。形状通道，转灰度也必须认得出（美术圣经 §9.3）。 */
export function lockBadge(g: Graphics, diameter: number, bg: Color, ringColor: Color, mark: Color): void {
    g.clear();
    disc(g, diameter, bg, ringColor, 3);
    const body = diameter * 0.3;
    g.fillColor = mark;
    g.roundRect(-body / 2, -diameter * 0.17, body, body * 0.85, 2);
    g.fill();
    g.strokeColor = mark;
    g.lineWidth = Math.max(3, Math.round(diameter / 12));
    g.arc(0, diameter * 0.07, body * 0.48, Math.PI, 0, false);
    g.stroke();
}

/** 未开放行的锁牌：圆角木牌 + 锁形，与角标同形状不同底。 */
export function lockChip(g: Graphics, size: number, bg: Color, border: Color, mark: Color): void {
    g.clear();
    rounded(g, size, size, 10, bg, border, 3);
    g.fillColor = mark;
    g.roundRect(-8, -10, 16, 13, 2);
    g.fill();
    g.strokeColor = mark;
    g.lineWidth = 4;
    g.arc(0, 5, 7, Math.PI, 0, false);
    g.stroke();
}

/** 右向三角（稿 05 / 06 的前后对照与等级阶梯）。 */
export function arrowRight(g: Graphics, width: number, height: number, fill: Color): void {
    g.clear();
    g.fillColor = fill;
    g.moveTo(-width / 2, height / 2);
    g.lineTo(width / 2, 0);
    g.lineTo(-width / 2, -height / 2);
    g.close();
    g.fill();
}

export function rgba(hex: string, alpha: number): Color {
    const color = rgb(hex);
    color.a = Math.round(255 * alpha);
    return color;
}

/** 与 rgb 同义，换个名字表明"这里要的是带 alpha 的稿值"。 */
export function withAlpha(hex: string, alpha: number): Color {
    return rgba(hex, alpha);
}
