// M06 的视觉令牌：色值与字号全部照抄《美术圣经》§2.2 主色板与 §8.4 字号表，
// 逐屏复用同一份，避免同一个色在五屏里写出三种写法。
//
// ⚠️ 这里集中的是**呈现层常量**，不是 ADR 0004 管的「数值与规则」——
// 那一条指的是价格、概率、节拍这类玩法数值（归 `config/`），改它们不碰代码；
// 而色值与像素尺寸属于界面资产，与 M04 写在场景里的坐标同类，不进 config。
//
// 坐标约定：设计稿是 @750×1334 的**左上角原点**绝对坐标，Cocos 的 UI 是
// 父节点中心原点、y 向上。换算集中在 place() 一处，逐屏代码里就不再出现减法。
import { Color, Label, Node, UITransform } from "cc";

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
