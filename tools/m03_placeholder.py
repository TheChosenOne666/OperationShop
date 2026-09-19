#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""M03 占位图生成器：为 `M03-素材投喂包.md` §1 的 13 件外部生成件产出**可替换的占位素材**。

用途与时限
----------
13 件外部生成件依赖逐件出图，是 M04 客户端工程的硬前置。本脚本让用户与出图工作**解耦**：
先用占位图把 M04 的**路径、像素尺寸、透明/不透明语义、安全边距**全部钉死，真素材到手后
用 `python tools/m03_ingest.py ingest --apply` 直接覆盖同名文件即可，工程侧零改动。

占位图**不是素材**，不得进入最终验收：
- 每张图上都印有资产名与交付尺寸，肉眼即知是占位；
- `assets/素材处理记录.md` §2.5 登记其来源与替换方式；
- 投喂包 §5 进度表记录的是**真素材**进度，占位图不计 ☐→✅。

设计约束（与 `tools/m03_ingest.py check` 的客观判据对齐，生成后必须自检通过）
----------------------------------------------------------------------------
- 尺寸取自投喂包 §1（经 `load_spec()` 解析），不另写一套；
- 描线/文字一律用 `#4A2F1E`（A7），全图不出现 `#000000`；
- 配色避开 A9 的蓝/青/紫/洋红 色相禁区（180°–330°），只用暖色与绿；
- 透明类（slt/shop/prp/app）必须留出透明区，且 shop/prp/app 内容不得贴画布边（D4 的 6% 留白）；
  九宫格件（ui/state）与 bg 铺满画布——留白是给立绘外摆的，面板没有外摆，铺满才能让「九宫格边」判据有像素可测；
- 5 张立绘共用同一套几何，使「立绘等高」组校验的顶/底边差为 0——这正是旧素材翻车的地方。

    python tools/m03_placeholder.py            # 生成 13 件到 assets/textures/<类>/
    python tools/m03_placeholder.py --force    # 已存在时也覆盖
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

try:
    from PIL import Image, ImageDraw, ImageFont
except ImportError as exc:
    sys.exit(f"缺少依赖：{exc}。本脚本需要 Pillow。")

import m03_ingest as ing  # noqa: E402  规格与策略的唯一来源

for _stream in (sys.stdout, sys.stderr):
    if hasattr(_stream, "reconfigure"):
        _stream.reconfigure(encoding="utf-8", errors="replace")

OUTLINE = (0x4A, 0x2F, 0x1E, 255)      # §1.7 A7 描线色，同时是全图最暗的颜色
TINT = {                                # 按类给底色，一眼分辨用途；全部避开 A9 色相禁区
    "bg": (0xF6, 0xE8, 0xCB),
    "slt": (0xE9, 0xD5, 0xAE),
    "shop": (0xF0, 0xDD, 0xB6),
    "prp": (0xD7, 0xE2, 0xB4),
    "app": (0xF3, 0xC9, 0x7A),
    "ui": (0xD9, 0xB2, 0x82),
    "state": (0xD9, 0xB2, 0x82),
    "ico": (0xF3, 0xC9, 0x7A),
}
STRIPE = (0xC8, 0xA9, 0x74)             # 斜纹带，比底色暗一档、仍高于描线色


def font(px: int):
    """尽量拿一个可缩放字体；拿不到就退回 PIL 内置位图字体（只影响标签大小，不影响判定）。"""
    try:
        return ImageFont.load_default(size=px)
    except TypeError:            # Pillow < 10.1 的 load_default 不收 size 参数
        return ImageFont.load_default()


def _fit(d: ImageDraw.ImageDraw, text: str, max_w: int, px: int):
    """把字号缩到能塞进 max_w。不缩的话长资产名会横向溢出内容框、顶破画布边，
    反而让占位图自己违反「不越界」判据。"""
    while px > 10:
        fnt = font(px)
        if d.textlength(text, font=fnt) <= max_w:
            return fnt
        px = int(px * 0.9)
    return font(px)


def label(d: ImageDraw.ImageDraw, box: tuple[int, int, int, int], asset: str, size: tuple[int, int]) -> None:
    """在内容框内居中印两行：资产名 / 交付尺寸。"""
    x0, y0, x1, y1 = box
    avail = x1 - x0 - 8                        # 左右各留 4px，文字绝不越框
    big = _fit(d, asset, avail, max(14, (y1 - y0) // 9))
    sub = f"PLACEHOLDER {size[0]}×{size[1]}"
    small = _fit(d, sub, avail, max(12, (y1 - y0) // 14))
    # 内置位图字体没有 .size 属性，取不到就按 11px 估行高。
    bh, sh = getattr(big, "size", 11), getattr(small, "size", 11)
    cy = y0 + (y1 - y0 - (bh + sh + 6)) // 2
    for text, fnt, hgt in ((asset, big, bh), (sub, small, sh + 6)):
        d.text((x0 + (x1 - x0 - d.textlength(text, font=fnt)) // 2, cy), text, font=fnt, fill=OUTLINE)
        cy += hgt


def stripes(d: ImageDraw.ImageDraw, box: tuple[int, int, int, int], step: int) -> None:
    """整幅画布上画斜纹；随后由圆角掩膜裁掉框外部分，所以这里不必担心越界。"""
    x0, y0, x1, y1 = box
    for off in range(-(y1 - y0), (x1 - x0), step * 2):
        d.line([(x0 + off, y1), (x0 + off + (y1 - y0), y0)], fill=STRIPE + (255,), width=max(3, step // 2))


def build(asset: str, target: dict) -> Image.Image:
    kind = ing.kind_of(asset)
    pol = ing.POLICY[kind]
    w, h = target["w"], target["h"]
    stroke = max(2, min(w, h) // 128)

    # 内容框：透明类按 D4 预留 6% 边距（立绘/道具不得贴边）；bg 铺满画布。
    # 九宫格件（ui/state）也铺满——6% 留白是给"立绘外摆"的，面板没有外摆；更要紧的是
    # 铺满后四条边才是不透明的，`check` 的「九宫格边」判据才有像素可测，不会空转成 WARN。
    if pol["transparent"] and pol["group"] != "ninepatch":
        pad = round(min(w, h) * 0.06)
        box = (pad, pad, w - pad, h - pad)
    else:
        box = (0, 0, w, h)
    # 九宫格件用大圆角（木条两端是圆的）：小圆角挖掉的透明区只有 0.03%，会卡在
    # 「真透明」判据的 0.5% 下限上；h/4 的圆角既留出约 1.3% 透明区，又保住四条边的中段不透明，
    # 让「九宫格边」判据仍然可测。
    radius = (min(w, h) // 4) if pol["group"] == "ninepatch" else max(4, min(w, h) // 24)

    shape = Image.new("L", (w, h), 0)
    ImageDraw.Draw(shape).rounded_rectangle(box, radius=radius, fill=255)

    fill = Image.new("RGBA", (w, h), TINT[kind] + (255,))
    stripes(ImageDraw.Draw(fill), (0, 0, w, h), max(10, min(w, h) // 16))

    img = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    img.paste(fill, (0, 0), shape)
    d = ImageDraw.Draw(img)
    # 描线画在内容框内侧一圈：Pillow 的 width 描边会压到 box 之外，先按线宽内缩，
    # 否则「不越界」判据（要求包围盒距画布边 >0）会被描边本身顶破。
    d.rounded_rectangle([box[0] + stroke, box[1] + stroke, box[2] - stroke, box[3] - stroke],
                        radius=radius, outline=OUTLINE, width=stroke)
    label(d, box, asset, (w, h))
    return img


def main() -> int:
    parser = argparse.ArgumentParser(description="M03 占位图生成器")
    parser.add_argument("--force", action="store_true", help="已存在的文件也覆盖")
    args = parser.parse_args()

    spec = ing.load_spec()
    written, kept = [], []
    for asset, target in sorted(spec.items(), key=lambda kv: kv[1]["index"]):
        dest = ing.TEXTURES / ing.POLICY[ing.kind_of(asset)]["subdir"] / f"{asset}.png"
        if dest.exists() and not args.force:
            kept.append(dest)
            continue
        dest.parent.mkdir(parents=True, exist_ok=True)
        build(asset, target).save(dest, optimize=True)
        written.append(dest)

    for dest in written:
        print(f"  生成 {dest.relative_to(ing.ROOT)}")
    if kept:
        print(f"\n⚠ 已存在、未覆盖 {len(kept)} 件（加 --force 才覆盖）：")
        for dest in kept:
            print(f"  · {dest.relative_to(ing.ROOT)}")
    print(f"\n共 {len(written)} 件占位图。现在跑 `python tools/m03_ingest.py check` 自检。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
