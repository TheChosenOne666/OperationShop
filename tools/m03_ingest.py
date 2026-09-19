#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""M03 素材入库管线：外部生成的纯色幕布底原图 → 抠像 → 定尺 → 客观自检 → 入库 → 回写进度。

依赖：Pillow + numpy（与既有 `.work/m03/build_assets.py` 同一套）；另用 scipy.ndimage 做
水印判据所需的连通域标记——scipy 缺失时该条判据自动跳过，不影响其余流程。

    python tools/m03_ingest.py spec              # 解析规格并与 ASTC 算式对账
    python tools/m03_ingest.py check             # 自检已入库件（含跨立绘等高的组校验）
    python tools/m03_ingest.py check a.png ...   # 自检指定文件
    python tools/m03_ingest.py ingest            # 预演 .work/incoming 下的候选，不写盘
    python tools/m03_ingest.py ingest a.png ...  # 预演指定原图（复查已退回样本用）
    python tools/m03_ingest.py ingest --apply    # 择优入库并勾掉投喂包 §5 进度表

**规格唯一来源是 `assets/M03-素材投喂包.md` §1 的逐件清单**——本脚本只解析、不复制尺寸与体积
目标。M03 已经吃过「同一个数抄四处、抄成三个不同值」的亏（开发规划文档待确认项 14），不再制造
第五个事实源；解析不到预期结构就报错退出，而不是静默用一套内置默认值。

本脚本**只判定客观项**（尺寸、真透明、键色残留、半透明毛边、贴边、正圆、单层占比、九宫格边
方差、纯黑、禁色带、描线色、ASTC 体积、原图是否单一品红底）。需要人眼或 OCR 的条目（正视图
透视、光源方向、时段、橱窗自发光、有无文字、橱窗是否全空、构图要素、五官）一律进「人工待勾」
清单，**不伪装成已通过**。
"""

from __future__ import annotations

import argparse
import math
import re
import sys
from pathlib import Path

try:
    import numpy as np
    from PIL import Image, ImageFilter
except ImportError as exc:
    sys.exit(f"缺少依赖：{exc}。本管线需要 Pillow 与 numpy。")

# 本仓库在中文 Windows 上开发：控制台默认 GBK，直接 print 中文会乱码、print 判定符号会抛
# UnicodeEncodeError。统一把输出流切成 UTF-8。
for _stream in (sys.stdout, sys.stderr):
    if hasattr(_stream, "reconfigure"):
        _stream.reconfigure(encoding="utf-8", errors="replace")

ROOT = Path(__file__).resolve().parent.parent
FEED_DOC = ROOT / "assets" / "M03-素材投喂包.md"
TEXTURES = ROOT / "assets" / "textures"
INCOMING = ROOT / ".work" / "incoming"

KEY_TOL = 96                       # 与幕布色的 L1 距离阈值，小于此判为背景
KEY_FLAT_MAX = 24                  # 边缘带对键色的 L1 九十五分位上限：超过即幕布有可见渐变/纹理
KEY_EDGE_MIN = 0.5                 # 边缘带里至少这么多像素落在键色 ±KEY_TOL 内，否则定位不到幕布
KEY_SAT_MIN = 60                   # 幕布色的最小通道极差：白/灰/黑底一律拒收（A13 棋盘格那类）
SPEC_KEY_RGB = (255, 0, 255)       # 《美术圣经》§1.7 A11 的推荐键色，偏离只提示不拦
OUTLINE_RGB = (0x4A, 0x2F, 0x1E)   # §1.7 A7：描线色，且不得出现纯黑
FORBIDDEN_HUE = (180.0, 330.0)     # §1.7 A9：蓝 / 青 / 紫 / 洋红 的色相禁区（度）
LUMA = np.array([0.2126, 0.7152, 0.0722])

# 每类的压缩策略与校验适用性：ASTC 块边长、是否 mipmap（美术圣经 §5.3.0）、是否要求透明背景、
# 是否允许内容铺满画框（底图/面板类按画框出图，贴边不是缺陷；立绘的外摆须留 6% D4）、
# 跨件组校验类别、入库子目录。
POLICY = {
    "bg": dict(block=8, mipmap=True, transparent=False, flush_ok=True, group=None, subdir="bg"),
    "slt": dict(block=6, mipmap=True, transparent=True, flush_ok=True, group=None, subdir="slt"),
    "shop": dict(block=6, mipmap=True, transparent=True, flush_ok=False, group="facade", subdir="shop"),
    "prp": dict(block=6, mipmap=False, transparent=True, flush_ok=False, group=None, subdir="prp"),
    "app": dict(block=6, mipmap=False, transparent=True, flush_ok=False, group=None, subdir="app"),
    "ui": dict(block=6, mipmap=False, transparent=True, flush_ok=True, group="ninepatch", subdir="ui"),
    "state": dict(block=6, mipmap=False, transparent=True, flush_ok=True, group="ninepatch", subdir="state"),
    "ico": dict(block=6, mipmap=False, transparent=True, flush_ok=False, group="circle", subdir="ico"),
}

# §1.7 / §1.8.3 中机器判不了、必须人眼的条目，按类列入「人工待勾」。
MANUAL = {
    "bg": ["B1 两层楼面与铺位数量顺序", "B2 二层栏杆 + 橘猫", "B3 楼梯位于一层两铺之间", "B4 雨棚 + 空白招牌板",
           "B5 两层等宽、墙垛分隔", "B6 铺前水平地面带（禁透视收敛）", "B7 不画 HUD", "B8 不画行人", "B9 不画三层/天空/远景"],
    "slt": ["C1 雨棚 + 木门框 + 左右玻璃橱窗", "C2 橱窗完全空", "C3 橱窗内壁暖米且亮着", "C4 两侧米白墙材质与主界面一致",
            "C5 一层版无户外桌椅/遮阳伞/花桶"],
    "shop": ["D1 严格单层门面", "D2 与空铺同墙体同雨棚结构", "D3 招牌留空木面无字", "D5 暖光从橱窗溢出",
             "雨棚配色：空铺/衣橱/书店纯暖绿、甜屋/咖啡橙白条、花束粉白条"],
    "prp": ["道具本体与《美术圣经》§7.7 的等级挂载一致"],
    "app": ["app_icon 在 512/192/48 三档缩放后仍可辨", "app_share 不含任何文字"],
    "ui": ["HUD 木条厚度与色值符合 §7.4", "九宫格四条切线由工程侧标定"],
    "state": ["纱罩为 35% 不透明度中性暖灰、无花纹（§7.3 S1-C）"],
    "ico": ["E1 正圆木章结构（外圈亮木环 + 内盘）", "E2 本体浅木色扁平浮雕",
            "E3 图标内无字符（金币 `$` 为唯一例外）", "E4 人脸类不画五官"],
    "ALL": ["A1 正视图平视、立柱垂直线互相平行", "A2 可见侧面 ≤ 宽度 8%", "A3 主光左上 30–45°、影落右下",
            "A4 傍晚 golden-hour 暖光（无夜景/正午/阴天）", "A6 只做接触影、无长投影",
            "A10 无金属高光/镜面反射/塑料反光", "A12 画面内无任何文字、字母、数字、logo、水印"],
}


def astc_kib(width: int, height: int, block: int, mipmap: bool) -> float:
    """《美术圣经》§5.3.0 的运行时压缩纹理算式：⌈w÷块⌉×⌈h÷块⌉×16 字节，开 mipmap 再 ×1.33。"""
    kib = math.ceil(width / block) * math.ceil(height / block) * 16 / 1024
    return kib * 1.33 if mipmap else kib


def kind_of(asset: str) -> str:
    return asset.split("_", 1)[0]


def load_spec() -> dict[str, dict]:
    """从投喂包 §1 逐件表解析 资产名 → {序号, 生成比例, 交付尺寸, 体积目标}。"""
    text = FEED_DOC.read_text(encoding="utf-8")
    spec: dict[str, dict] = {}
    pattern = (r"^\|\s*(\d+)\s*\|\s*`([a-z0-9_]+)`[^|]*\|\s*([0-9:]+)\s*\|"
               r"\s*(\d+)×(\d+)[^|]*\|\s*≤\s*(\d+(?:\.\d+)?)\s*KiB")
    for idx, name, ratio, w, h, budget in re.findall(pattern, text, re.MULTILINE):
        spec[name] = dict(index=int(idx), ratio=ratio, w=int(w), h=int(h), budget_kib=float(budget))
    if not spec:
        sys.exit(f"未能从 {FEED_DOC.name} §1 解析出任何资产——表结构可能已改动，请核对脚本里的正则后重试。")
    unknown = sorted({kind_of(n) for n in spec} - set(POLICY))
    if unknown:
        sys.exit(f"投喂包出现未登记类别的资产 {unknown}：请在 POLICY 中补充该类的压缩策略与校验适用性。")
    return spec


# ---------------------------------------------------------------- 形态学小工具（形状恒为 H×W）
def _padded(mask: np.ndarray) -> np.ndarray:
    return np.pad(mask, 1, constant_values=False)


def majority(mask: np.ndarray, min_true: int = 5) -> np.ndarray:
    """3×3 多数表决，用于消孤立碎点与孔洞。输入输出均为 H×W。

    在加过 1px 边框的 p 空间上卷积后须取内区 [1:-1,1:-1]：切 [:H,:W] 会让 3×3 窗口
    中心偏移一格，等效于把整张掩膜平移 1px。
    """
    p = _padded(mask).astype(np.uint8)
    acc = np.zeros_like(p, dtype=np.uint8)
    for dy in (-1, 0, 1):
        for dx in (-1, 0, 1):
            acc += np.roll(np.roll(p, dy, axis=0), dx, axis=1)
    return (acc >= min_true)[1:-1, 1:-1]


def erode(mask: np.ndarray) -> np.ndarray:
    """3×3 全真腐蚀，用于收紧主体轮廓、剥掉键色描边。输入输出均为 H×W。"""
    p = _padded(mask)
    out = np.ones(p.shape, dtype=bool)
    for dy in (-1, 0, 1):
        for dx in (-1, 0, 1):
            out &= np.roll(np.roll(p, dy, axis=0), dx, axis=1)
    return out[1:-1, 1:-1]


# ---------------------------------------------------------------- 测量与判定
def detect_key(img: Image.Image) -> dict:
    """从画面边缘带实测幕布色与幕布质量。

    《美术圣经》§1.7 A11 写的是 `#FF00FF`，但生图模型对色名的理解不可靠——实测豆包连续两次
    把它画成玫红 RGB(205,51,81)／(247,64,87)，自产图则画成肉粉 (237,151,141)。A11 真正要的是
    「一块能干净抠掉的均匀高饱和幕布」，不是那串数值，故改为**按图取键色**。

    键色取**边缘带的中位数**，平整度取**边缘带像素到键色 L1 距离的 95 分位**。校准依据
    `.work/qa-m03/calibrate_flat.py`：三张真图（两张豆包、一张自产，幕布均为人工确认的平整纯色）
    实测 6.0／11.0／16.0；在同样三张图上合成 8% 高度渐变 → 31／39／55，合成 5% 明度的水平条纹
    纹理 → 23／27／33，合成 ±6 通道噪点 → 17／21／22。阈值取 24 落在「真图最高 16」与「渐变最低
    31」之间，且容忍了等价于 JPG 压缩噪点的 ±6 抖动。

    只用边缘带、且用分位数而非均值，是为了不被角落杂物带偏：平台水印通常只占边缘像素的百分之几，
    均值/max 会把它误报成「幕布有渐变」，95 分位不会。
    """
    arr = np.asarray(img.convert("RGB")).astype(np.int32)
    h, w = arr.shape[:2]
    band = max(6, min(h, w) // 64)
    edge = np.concatenate([arr[:band].reshape(-1, 3), arr[-band:].reshape(-1, 3),
                           arr[:, :band].reshape(-1, 3), arr[:, -band:].reshape(-1, 3)])
    color = np.median(edge, axis=0)
    edge_dist = np.abs(edge - color).sum(axis=1)
    dist = np.abs(arr - color).sum(axis=2)
    return dict(color=color,
                flat=float(np.percentile(edge_dist, 95)),
                edge_share=float((edge_dist < KEY_TOL).mean()),
                coverage=float((dist < KEY_TOL).mean()),
                sat=float(color.max() - color.min()))


def stray_blobs(mask: np.ndarray, keep_ratio: float = 0.05) -> tuple[np.ndarray, float, bool]:
    """剔除不连到主体的散块（平台水印/角标/幕布杂物），返回（清理后掩膜，被剔面积占比，是否有块落在四角带）。

    保留面积 ≥ 最大块 `keep_ratio` 的连通块——彩灯串这类合法素材本身可能有分离的较大部件，
    不能一律按散块删掉。水印通常是最大块面积的千分之几，会被稳定剔除。
    平台角标几乎总在画面角落，故单独标记，供上层判断是「水印」还是「杂物」。
    """
    from scipy import ndimage  # 局部导入：仅本判据用到，scipy 缺失时由调用方降级处理
    labels, count = ndimage.label(mask)
    if count <= 1:
        return mask, 0.0, False
    sizes = ndimage.sum(mask, labels, range(1, count + 1))
    main = int(sizes.argmax())
    keep = sizes >= sizes[main] * keep_ratio
    h, w = mask.shape
    stray_area, corner_hit = 0.0, False
    for lab in range(1, count + 1):
        if keep[lab - 1]:
            continue
        area = float(sizes[lab - 1]) / mask.size
        stray_area += area
        ys, xs = np.nonzero(labels == lab)
        if (ys.min() < h * 0.15 or ys.max() > h * 0.85) and (xs.min() < w * 0.15 or xs.max() > w * 0.85):
            corner_hit = True
    return np.isin(labels, [i + 1 for i in range(count) if keep[i]]), stray_area, corner_hit


def measure(img: Image.Image, key: np.ndarray | None = None) -> dict:
    """采集客观测量值。

    传入 `key`（幕布色）时按**原图**统计，主体由「非幕布色」界定——原图整幅不透明，若按 alpha
    统计会被背景污染。不传则按已抠好 alpha 的入库件统计，主体＝不透明像素。
    """
    rgba = np.asarray(img.convert("RGBA")).astype(np.int32)
    rgb, alpha = rgba[:, :, :3], rgba[:, :, 3]
    raw = key is not None
    if raw:
        subject = np.abs(rgb - key).sum(axis=2) >= KEY_TOL
    else:
        subject = alpha >= 250
    total = alpha.size
    ys, xs = np.nonzero(subject)
    bbox = (int(xs.min()), int(ys.min()), int(xs.max()) + 1, int(ys.max()) + 1) if len(xs) else (0, 0, 0, 0)
    px = rgb[subject]
    neutral = ((rgb.min(axis=2) > 215) & ((rgb.max(axis=2) - rgb.min(axis=2)) < 14))

    out = dict(raw=raw, size=img.size, bbox=bbox,
               opaque_ratio=subject.sum() / total,
               soft_alpha=((alpha > 0) & (alpha < 255)).sum() / total,
               key_residue=0.0, forbidden_hue=0.0, pure_black=0, outline_dist=0.0,
               raw_neutral=float(neutral.mean()))
    if len(px) == 0:
        return out
    r, g, b = px[:, 0], px[:, 1], px[:, 2]
    hue = (np.degrees(np.arctan2(np.sqrt(3) * (g - b), 2 * r - g - b)) + 360) % 360
    if key is not None:
        out["key_residue"] = float((np.abs(px - key).sum(axis=1) < 150).mean())
    out["forbidden_hue"] = float(((hue >= FORBIDDEN_HUE[0]) & (hue <= FORBIDDEN_HUE[1])).mean())
    out["pure_black"] = int(((px == 0).all(axis=1)).sum())
    darkest = px[np.argsort(px @ LUMA)[: max(1, len(px) // 200)]]
    out["outline_dist"] = float(np.abs(darkest.mean(axis=0) - np.array(OUTLINE_RGB)).sum())
    return out


def evaluate(img: Image.Image, asset: str, target: dict, is_raw: bool) -> list[tuple[str, str, str]]:
    """返回 [(编号, 级别, 说明)]，级别 ∈ PASS / WARN / FAIL。"""
    pol = POLICY[kind_of(asset)]
    backdrop = detect_key(img) if is_raw else None
    m = measure(img, key=backdrop["color"] if backdrop else None)
    w, h = m["size"]
    x0, y0, x1, y1 = m["bbox"]
    res: list[tuple[str, str, str]] = []
    add = res.append

    if is_raw:
        # 投喂包 §3：原图须给**原始高分辨率**（短边 ≥1024），缩放与裁切由管线统一做，先缩后裁会损失质量。
        short = min(w, h)
        add(("原图分辨率", "PASS" if short >= 1024 else "FAIL",
             f"短边 {short}px（要求 ≥1024；请勿自行缩放到交付尺寸）"))
        color, flat, sat = backdrop["color"], backdrop["flat"], backdrop["sat"]
        edge_share, coverage = backdrop["edge_share"], backdrop["coverage"]
        rgb_txt = "RGB({:.0f},{:.0f},{:.0f})".format(*color)
        add(("幕布可定位", "PASS" if edge_share >= KEY_EDGE_MIN else "FAIL",
             f"边缘带 {edge_share * 100:.1f}% 的像素落在实测键色 ±{KEY_TOL} 内（阈值 {KEY_EDGE_MIN * 100:.0f}%；"
             f"不足说明画面边缘被主体占满或底色不止一种，无法定幕布色）"))
        add(("幕布平整", "PASS" if flat < KEY_FLAT_MAX else "FAIL",
             f"实测幕布 {rgb_txt}，边缘带到键色 L1 的 95 分位 {flat:.1f}（阈值 {KEY_FLAT_MAX}；"
             f"真图实测 6–16，合成 8% 渐变 31–55）"))
        add(("幕布高饱和", "PASS" if sat >= KEY_SAT_MIN else "FAIL",
             f"通道极差 {sat:.0f}（阈值 {KEY_SAT_MIN}；白/灰/黑底即 A13 那类假透明，一律拒收）"))
        add(("幕布覆盖", "PASS" if coverage >= 0.05 else "FAIL",
             f"幕布占全图 {coverage * 100:.1f}%；浅中性占 {m['raw_neutral'] * 100:.1f}%"))
        mask = np.abs(np.asarray(img.convert("RGB")).astype(np.int32) - color).sum(axis=2) >= KEY_TOL
        mask, stray, corner = stray_blobs(mask)
        add(("散块清理", "PASS" if stray == 0 else ("FAIL" if stray > 0.02 else "WARN"),
             f"将剔除非主体散块 {stray * 100:.2f}%"
             + ("（落在画面角落＝平台水印/角标，已自动移除）" if corner and stray else "")
             + ("；面积过大，疑似真被裁进画面的杂物，请人工确认" if stray > 0.02 else "")))
        if np.abs(color - np.array(SPEC_KEY_RGB)).sum() > 3 * KEY_TOL:
            add(("键色偏离", "WARN", f"A11 推荐 `#FF00FF`，实测 {rgb_txt}——可抠图，不拦；入库件将按实测色键控"))
        # 贴边判定必须用**清理后**的掩膜：否则角落水印碰着画面边，会把一张合格图误判成"主体被裁"。
        ys, xs = np.nonzero(mask)
        if len(xs):
            margin = min(xs.min(), ys.min(), w - 1 - xs.max(), h - 1 - ys.max())
            add(("主体完整", "PASS" if margin > 0 else "FAIL",
                 f"主体距画布最近边 {margin}px；贴边＝被裁掉一块，或幕布上有投影溢出"))
        else:
            add(("主体完整", "FAIL", "清理后画面无主体"))
    else:
        add(("尺寸", "PASS" if (w, h) == (target["w"], target["h"]) else "FAIL",
             f"{w}×{h}，目标 {target['w']}×{target['h']}"))
        kib = astc_kib(target["w"], target["h"], pol["block"], pol["mipmap"])
        add(("体积", "PASS" if kib <= target["budget_kib"] else "FAIL",
             f"ASTC {pol['block']}×{pol['block']}{' +mip' if pol['mipmap'] else '     '} = {kib:.1f} KiB / 上限 {target['budget_kib']:.0f} KiB"))

    if pol["transparent"] and not is_raw:
        add(("真透明", "PASS" if m["opaque_ratio"] < 0.995 else "FAIL",
             f"不透明像素占 {m['opaque_ratio'] * 100:.1f}%（要求存在透明区）"))
        add(("无半透明毛边", "PASS" if m["soft_alpha"] <= 0.03 else "WARN",
             f"0<alpha<255 占 {m['soft_alpha'] * 100:.2f}%（A14 阈值 3%）"))
        add(("键色残留", "PASS" if m["key_residue"] <= 0.005 else "FAIL",
             f"主体内近品红像素 {m['key_residue'] * 100:.2f}%（抠像/去溢色不净）"))
        margin = min(x0, y0, w - x1, h - y1)
        if not pol["flush_ok"]:
            add(("不越界", "PASS" if margin > 0 else "FAIL",
                 f"内容包围盒距画布最近边 {margin}px（立绘外摆须留边，D4 ≤6%）"))
    if kind_of(asset) == "ico" and not is_raw:
        bw, bh = x1 - x0, y1 - y0
        ratio = bw / bh if bh else 0.0
        add(("正圆", "PASS" if abs(ratio - 1) <= 0.02 else "WARN", f"内容长宽比 {ratio:.3f}（要求 1:1 ±2%）"))
    # 注：§1.8.3 的「立面高度占比 ≥88%」**不做机器判定**。它对「内容纵横比 ≠ 画布纵横比」的
    # 合格素材会系统性误报（等比适配 + 透明留白后，内容高度上限即 1−2×6% = 88%），
    # 换个画布比例就给出不同的数——没有判定力的检查比没有检查更糟。单层与否留在 D1 人工项。
    if pol["group"] == "ninepatch" and not is_raw:
        # 只在**不透明**的边像素上算方差：全透明的边行方差恒为 0，会让该检查空转通过。
        arr = np.asarray(img.convert("RGBA")).astype(np.float64)
        opaque = arr[:, :, 3] >= 250
        rows = [(arr[0], opaque[0]), (arr[-1], opaque[-1]), (arr[:, 0], opaque[:, 0]), (arr[:, -1], opaque[:, -1])]
        spreads, empty = [], 0
        for edge, mask in rows:
            if mask.sum() < max(4, 0.02 * len(mask)):
                empty += 1
                continue
            spreads.append(edge[mask].std(axis=0).mean())
        if spreads:
            spread = float(np.mean(spreads))
            note = f"四边不透明段的标准差均值 {spread:.1f}（{4 - empty}/4 边可测）"
            add(("九宫格边", "PASS" if spread <= 26 else "WARN", note + ("；其余边全透明，无法判定" if empty else "")))
        else:
            add(("九宫格边", "WARN", "四条边均无足够不透明像素，该检查不适用——九宫格可拉伸性须人工确认"))

    add(("无纯黑", "PASS" if m["pure_black"] == 0 else "FAIL", f"`#000000` 像素 {m['pure_black']} 个（A7）"))
    add(("禁色带", "PASS" if m["forbidden_hue"] <= 0.005 else "WARN",
         f"蓝/青/紫/洋红 色相占主体 {m['forbidden_hue'] * 100:.2f}%（A9，背景键色除外）"))
    add(("描线色", "PASS" if m["outline_dist"] <= 90 else "WARN",
         f"最暗 0.5% 像素均色与 `#4A2F1E` 的 L1 距离 {m['outline_dist']:.0f}（A7）"))
    return res


# ---------------------------------------------------------------- 抠像与定尺
def despill(arr: np.ndarray, key: np.ndarray, strength: float = 1.0) -> np.ndarray:
    """沿幕布色的色度方向把靠近幕布的像素推离，消除边缘溢色。

    对品红幕布等效于经典的「压 R/B、抬 G」；对玫红等实测色同样成立，故不写死颜色。
    """
    center = float(key.mean())
    direction = key.astype(np.float64) - center
    norm = np.linalg.norm(direction)
    if norm < 1:
        return arr
    direction /= norm
    projection = (arr - center) @ direction
    proximity = np.clip(1.0 - np.abs(arr - key).sum(axis=2) / (KEY_TOL * 3.0), 0.0, 1.0)
    shift = np.clip(projection, 0, None) * proximity * strength
    return arr - shift[:, :, None] * direction


def key_and_fit(src: Path, target: dict, asset: str, feather: float = 0.8) -> Image.Image:
    """按实测幕布色键控 → 去噪 → 1px 腐蚀 → 去溢色 → 羽化 → 包围盒裁切 → 等比适配到交付画布。"""
    img = Image.open(src).convert("RGB")
    key = detect_key(img)["color"]
    arr = np.asarray(img).astype(np.float64)
    subject = np.abs(arr - key).sum(axis=2) >= KEY_TOL
    subject = majority(subject, min_true=5)          # 先去幕布碎点
    subject = erode(subject)                         # 再剥 1px 幕布描边（A14 防毛边）
    subject = majority(subject, min_true=4)          # 补回被过度腐蚀的小面积
    subject, stray, _ = stray_blobs(subject)         # 剔除平台水印/角标等不连主体的散块

    alpha = Image.fromarray(subject.astype(np.uint8) * 255)   # 单通道 → Pillow 自推为 "L"
    if feather > 0:
        alpha = alpha.filter(ImageFilter.GaussianBlur(feather))
    body = Image.fromarray(np.clip(despill(arr, key), 0, 255).astype(np.uint8)).convert("RGBA")
    body.putalpha(alpha)

    bbox = alpha.getbbox()
    if bbox is None:
        raise ValueError(f"{src.name}：键控后主体为空——幕布判定过宽，或整幅是同一纯色")
    body = body.crop(bbox)

    canvas = Image.new("RGBA", (target["w"], target["h"]), (0, 0, 0, 0))
    if POLICY[kind_of(asset)]["transparent"]:
        pad = round(min(canvas.size) * 0.06)         # 立绘/道具类预留 6% 透明边距（D4）
        avail_w, avail_h = canvas.width - 2 * pad, canvas.height - 2 * pad
    else:
        avail_w, avail_h = canvas.width, canvas.height   # 全幅背景类：铺满画布，不留透明边
    scale = min(avail_w / body.width, avail_h / body.height)
    body = body.resize((max(1, round(body.width * scale)), max(1, round(body.height * scale))), Image.LANCZOS)
    canvas.paste(body, ((canvas.width - body.width) // 2, (canvas.height - body.height) // 2), body)
    return canvas


def score(path: Path) -> float:
    """多候选择优：越小越好。键色残留权重最高，其次毛边与禁色带。"""
    m = measure(Image.open(path))
    return m["key_residue"] * 400 + m["soft_alpha"] * 30 + m["forbidden_hue"] * 200


def report(label: str, results: list[tuple[str, str, str]], indent: str = "  ") -> int:
    fails = sum(1 for _, lvl, _ in results if lvl == "FAIL")
    print(f"{indent}{label}  {'❌ 有 ' + str(fails) + ' 项不通过' if fails else '✅ 客观项通过'}")
    for cid, lvl, detail in results:
        print(f"{indent}  {'·⚠✗'[['PASS', 'WARN', 'FAIL'].index(lvl)]} {cid:<13}{detail}")
    return fails


# ---------------------------------------------------------------- 子命令
def cmd_spec(_: argparse.Namespace) -> int:
    spec = load_spec()
    print(f"投喂包 §1 解析出 {len(spec)} 件，逐件与《美术圣经》§5.3.0 算式对账：\n")
    bad = 0
    for name, t in sorted(spec.items(), key=lambda kv: kv[1]["index"]):
        pol = POLICY[kind_of(name)]
        kib = astc_kib(t["w"], t["h"], pol["block"], pol["mipmap"])
        ok = kib <= t["budget_kib"]
        bad += not ok
        print(f"  {t['index']:>2} {name:<20} {t['w']}×{t['h']:<6} ASTC {pol['block']}×{pol['block']}"
              f"{' +mip' if pol['mipmap'] else '     '}  算得 {kib:7.1f} KiB ≤ 上限 {t['budget_kib']:5.0f} KiB  {'✅' if ok else '❌'}")
    print(f"\n{'全部自洽：投喂包的体积目标可由该算式复现。' if not bad else f'⚠ {bad} 件与投喂包目标不符——两处有一处写错了。'}")
    return 1 if bad else 0


def cmd_check(args: argparse.Namespace) -> int:
    spec = load_spec()
    # `_archive/` 下是同名的历史件（旧素材与新件共用资产名），缺省自检必须跳过，
    # 否则一件会被查两遍、且立绘等高组校验会把两套基线混在一起算。
    paths = ([Path(p) for p in args.paths] if args.paths
             else [p for p in sorted(TEXTURES.rglob("*.png")) if "_archive" not in p.parts])
    total, checked, skipped = 0, 0, 0
    by_group: dict[str, list[Path]] = {}
    for path in paths:
        asset = re.sub(r"_c\d+$", "", path.stem)
        if asset not in spec:
            skipped += 1
            continue
        checked += 1
        total += report(f"{asset}  [{path.relative_to(ROOT)}]",
                        evaluate(Image.open(path), asset, spec[asset], is_raw=False))
        group = POLICY[kind_of(asset)]["group"]
        if group:
            by_group.setdefault(group, []).append(path)

    for group, items in sorted(by_group.items()):
        if group == "facade" and len(items) >= 2:
            boxes = [(p.stem, measure(Image.open(p))) for p in items]
            h = boxes[0][1]["size"][1]
            tops = [m["bbox"][1] / m["size"][1] for _, m in boxes]
            bots = [m["bbox"][3] / m["size"][1] for _, m in boxes]
            spread = max(max(tops) - min(tops), max(bots) - min(bots))
            total += report("组校验 / 立绘等高",
                            [("等高", "PASS" if spread <= 0.03 else "FAIL",
                              f"{len(boxes)} 张顶/底边最大差 {spread * 100:.1f}%（阈值 ≤3%）：" + "、".join(n for n, _ in boxes))])
    print(f"\n检查 {checked} 件（另有 {skipped} 件不在 13 件清单内，已跳过），FAIL 合计 {total} 项。")
    return 1 if total else 0


def cmd_ingest(args: argparse.Namespace) -> int:
    spec = load_spec()
    if args.paths:
        # 显式给路径时不受收件目录限制：便于复查已退回的样本，无需把它们搬回来。
        candidates = [Path(p) for p in args.paths]
    elif not INCOMING.is_dir():
        print(f"收件目录不存在：{INCOMING.relative_to(ROOT)}\n"
              f"建好后把生成的 PNG 放进去（文件名＝资产名，同一件多候选加 _c1/_c2 后缀）。")
        return 1
    else:
        candidates = sorted(INCOMING.glob("*.png"))
    if not candidates:
        print(f"{INCOMING.relative_to(ROOT)} 里没有 PNG。投喂包 §3 要求格式为 PNG（JPG 会在品红底上产生压缩噪点，直接拒收）。")
        return 0

    winners: dict[str, tuple[float, Path]] = {}
    for path in candidates:
        asset = re.sub(r"_c\d+$", "", path.stem)
        if asset not in spec:
            print(f"✗ {path.name}：`{asset}` 不在投喂包 §1 的 13 件清单内，跳过。")
            continue
        img = Image.open(path)
        results = evaluate(img, asset, spec[asset], is_raw=True)
        fails = report(f"{path.name}", results)
        if fails:
            print("      → 退回重出，不入库。\n")
            continue
        fitted = key_and_fit(path, spec[asset], asset)
        results2 = evaluate(fitted, asset, spec[asset], is_raw=False)
        report(f"{asset}（抠像定尺后）", results2, indent="      ")
        if not any(lvl == "FAIL" for _, lvl, _ in results2):
            s = score(path)
            if asset not in winners or s < winners[asset][0]:
                winners[asset] = (s, path)
        print()

    if args.apply:
        for asset, (_, path) in sorted(winners.items()):
            dest = TEXTURES / POLICY[kind_of(asset)]["subdir"] / f"{asset}.png"
            dest.parent.mkdir(parents=True, exist_ok=True)
            key_and_fit(path, spec[asset], asset).save(dest, optimize=True)
            print(f"  已入库 {dest.relative_to(ROOT)}")
        flip_progress(list(winners))
    print(f"可入库 {len(winners)} / 清单 {len(spec)} 件。"
          f"{'已写盘。' if args.apply else '预演模式——加 --apply 才写盘。'}")
    for asset in sorted(winners):
        print(f"\n{asset} 人工待勾：")
        for item in MANUAL["ALL"] + MANUAL[kind_of(asset)]:
            print(f"  ☐ {item}")
    return 0


def flip_progress(assets: list[str]) -> None:
    """把投喂包 §5 进度表里对应资产的「已投喂 / 已入库」两格勾上。"""
    text = FEED_DOC.read_text(encoding="utf-8")
    for asset in assets:
        row = re.compile(r"^(\|\s*\d+\s*\|\s*`" + re.escape(asset) + r"`[^|]*\|\s*)(☐|✅)(\s*\|\s*)(☐|✅)(\s*\|)", re.MULTILINE)
        text = row.sub(lambda g: f"{g[1]}✅{g[3]}✅{g[5]}", text)
    FEED_DOC.write_text(text, encoding="utf-8")
    print(f"  已回写投喂包 §5 进度表 {len(assets)} 行")


def main() -> int:
    parser = argparse.ArgumentParser(description="M03 素材入库管线")
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("spec", help="解析规格并与 ASTC 算式对账").set_defaults(func=cmd_spec)
    p_check = sub.add_parser("check", help="客观自检")
    p_check.add_argument("paths", nargs="*", help="缺省为 assets/textures 下全部 PNG（只跑 13 件清单内的）")
    p_check.set_defaults(func=cmd_check)
    p_ing = sub.add_parser("ingest", help="处理 .work/incoming 下的候选")
    p_ing.add_argument("paths", nargs="*", help="显式指定原图路径（缺省为收件目录下的全部 PNG）")
    p_ing.add_argument("--apply", action="store_true", help="真正写盘并回写进度表")
    p_ing.set_defaults(func=cmd_ingest)
    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
