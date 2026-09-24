#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""M04 构建入口：CocosCreator 无头构建微信小游戏 → 补 AppID → 报包体 → （可选）开开发者工具。

为什么需要这个脚本（三件事都不做就会每次踩坑）
----------------------------------------------
1. **AppID 必须由构建后补丁写回。** Cocos 3.8.8 的 CLI **无法**设置 appid：
   `--build "appid=…"` 与手写 `settings/v2/packages/builder.json` 均不生效（实测日志里真实
   参数住在 `packages.wechatgame.appid`，两条路都改不到它），产物始终是 Cocos 默认测试号
   `wx6ac3f5090a6b99c5`，开发者工具于是报「登录用户不是该小程序的开发者」而拒绝打开。
2. **起始场景 uuid 不能写死。** uuid 存在 `Boot.scene.meta` 里，由该文件唯一决定；
   从 meta 读出来传给它自己，写死反而会在换机/重建后对不上。
3. **包体数字是 M04 验收第 4 条**，每次构建都要有，所以顺手算出来，不再手工量。

工具链路径**不写进仓库**——每台机器装的位置不同。优先读环境变量，其次探测标准安装位置：
    COCOS_CREATOR     指向 CocosCreator.exe
    WX_DEVTOOLS_CLI   指向微信开发者工具的 cli.bat

    python tools/m04_build.py                 # 构建 + 补 AppID + 报体积
    python tools/m04_build.py --open          # 再顺带打开微信开发者工具
    python tools/m04_build.py --size-only     # 不构建，只量现有产物
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

for _stream in (sys.stdout, sys.stderr):
    if hasattr(_stream, "reconfigure"):
        _stream.reconfigure(encoding="utf-8", errors="replace")

ROOT = Path(__file__).resolve().parent.parent
PROJECT = ROOT / "NewProject"
CLIENT_CONFIG = ROOT / "config" / "client.json"
START_SCENE = PROJECT / "assets" / "scene" / "Boot.scene"
BUILD_DIR = PROJECT / "build" / "wechatgame"
PLATFORM = "wechatgame"
MAIN_PACKAGE_LIMIT_KIB = 4096          # 微信官方硬上限：主包不超过 4M（平台事实核对 §1.1）

COCOS_CANDIDATES = (                    # 官方 Dashboard 的默认安装位置
    r"C:\ProgramData\cocos\editors\Creator\3.8.8\CocosCreator.exe",
    r"C:\Program Files\Cocos\editors\Creator\3.8.8\CocosCreator.exe",
)
DEVTOOLS_CANDIDATES = (
    r"C:\Program Files (x86)\Tencent\微信web开发者工具\cli.bat",
    r"C:\Program Files\Tencent\微信web开发者工具\cli.bat",
)


def resolve(label: str, env_var: str, candidates: tuple[str, ...]) -> Path:
    """环境变量优先，其次探测标准位置；都找不到就把两条路都告诉用户，不静默失败。"""
    env = os.environ.get(env_var)
    if env:
        p = Path(env)
        if p.is_file():
            return p
        sys.exit(f"{label}：环境变量 {env_var} 指向的文件不存在：{env}")
    for cand in candidates:
        p = Path(cand)
        if p.is_file():
            return p
    sys.exit(
        f"{label}：没找到可执行文件。请设环境变量 {env_var} 指向它，"
        f"或装在以下位置之一：\n  " + "\n  ".join(candidates)
    )


def read_client_config() -> dict:
    if not CLIENT_CONFIG.is_file():
        sys.exit(f"缺少 {CLIENT_CONFIG.relative_to(ROOT)}——微信 AppID 与项目名从那里读。")
    cfg = json.loads(CLIENT_CONFIG.read_text(encoding="utf-8"))
    for key in ("wechatAppid", "projectName"):
        if not cfg.get(key):
            sys.exit(f"{CLIENT_CONFIG.name} 缺少 `{key}` 字段。")
    return cfg


def write_build_config(cfg: dict) -> None:
    """生成 assets/scripts/BuildConfig.ts（不入库）。

    构建模式从环境变量 MALL_AUTH_MODE 读（dev 默认 / wechat），必须与启动服务端时一致：
      · dev    —— 注入开发令牌（只走环境变量 MALL_DEV_TOKEN，必须与服务端一致，否则一律 401）。
                 为什么不让它进仓库：小游戏的包是明文可读的，提交进版本库等于公开它。
      · wechat —— **不注入任何令牌**：客户端启动时走 wx.login 换会话令牌（见 ApiClient.ts）。
                 把开发令牌打进来没有任何用处（服务端 wechat 模式拒绝它），平白多一个泄露面。
    """
    token = os.environ.get("MALL_DEV_TOKEN", "")
    base = os.environ.get("MALL_BASE_URL", "http://127.0.0.1:18081")
    mode = (os.environ.get("MALL_AUTH_MODE") or "dev").strip()
    if mode not in ("dev", "wechat"):
        sys.exit(f"MALL_AUTH_MODE 只能是 dev 或 wechat，当前是 {mode!r}。")
    if mode == "dev" and not token:
        print("  ⚠ 未设 MALL_DEV_TOKEN——生成的客户端不带令牌，请求服务端会全部 401。")
    target = PROJECT / "assets" / "scripts" / "BuildConfig.ts"
    target.parent.mkdir(parents=True, exist_ok=True)
    lines = [
        "// 由 tools/m04_build.py 生成，请勿手改、勿提交（见 .gitignore）。",
        "// 模式与令牌只走环境变量：MALL_AUTH_MODE（dev/wechat）+ MALL_DEV_TOKEN（仅 dev 需要）。",
        "export const BUILD_CONFIG = {",
        f'    baseUrl: "{base}",',
        f'    authMode: "{mode}",',
    ]
    if mode == "dev":
        lines.append(f'    devToken: "{token}",')
    lines.append("};")
    target.write_text("\n".join(lines) + "\n", encoding="utf-8")
    injected = f"token={'已注入' if token else '（空）'}" if mode == "dev" else "token=（不注入，走微信登录）"
    print(f"▶ 生成 BuildConfig.ts：baseUrl={base} mode={mode} {injected}")


def start_scene_uuid() -> str:
    meta = START_SCENE.with_suffix(START_SCENE.suffix + ".meta")
    if not meta.is_file():
        sys.exit(f"找不到 {meta.relative_to(ROOT)}——起始场景尚未被编辑器导入（先开一次工程）。")
    return json.loads(meta.read_text(encoding="utf-8"))["uuid"]


def build(creator: Path, uuid: str) -> int:
    cmd = [str(creator), "--project", str(PROJECT),
           "--build", f"platform={PLATFORM};startScene={uuid}"]
    print("▶ 无头构建（CocosCreator CLI）")
    print(f"  {' '.join(cmd[:2])} … --build \"platform={PLATFORM};startScene={uuid[:8]}…\"")
    proc = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")
    if proc.returncode != 0:
        # 已查清（2026-09-19）：日志里的 SIGTERM in task build-script 是编译子进程**收尾**时
        # 被杀的信号，同一份日志随后即 `run build task 打包脚本 success`，且工程内 TS 确实
        # 出现在产物 `assets/main/index.js` 中。故退出码非 0 不代表脚本没编译。
        # 但仍不静默放过：真正的编译错误也会走到这里，需另用产物内容复核。
        print(f"  ⚠ 构建退出码 {proc.returncode}（SIGTERM 属收尾噪音；请核对产物内是否含工程脚本）")
    return proc.returncode


def patch_appid(cfg: dict) -> None:
    """把 AppID 与项目名写回构建产物。Cocos 每次构建都会重写此文件，故补丁必须在构建之后。"""
    target = BUILD_DIR / "project.config.json"
    if not target.is_file():
        sys.exit(f"构建产物缺失：{target.relative_to(ROOT)}——构建没成功，先看 temp/builder/log。")
    data = json.loads(target.read_bytes().decode("utf-8-sig"))   # Cocos 写的是无 BOM UTF-8，容错两种
    before = data.get("appid")
    data["appid"] = cfg["wechatAppid"]
    data["projectname"] = cfg["projectName"]
    target.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
    flag = "✅ 已改" if before != cfg["wechatAppid"] else "✅ 本就是"
    print(f"▶ AppID 补丁：{flag} {before} → {cfg['wechatAppid']}（projectname={cfg['projectName']}）")


def report_size() -> None:
    """M04 验收第 4 条：主包体积要有明确数字。"""
    files = [f for f in BUILD_DIR.rglob("*") if f.is_file()]
    total = sum(f.stat().st_size for f in files)
    kib = total / 1024
    print("\n▶ 包体（构建产物，未压缩为平台包前的目录字节数）")
    for d in sorted(p for p in BUILD_DIR.iterdir() if p.is_dir()):
        s = sum(f.stat().st_size for f in d.rglob("*") if f.is_file())
        if s > 16 * 1024:
            print(f"    {d.name:<14} {s / 1024:8.1f} KiB")
    print(f"  {'合计':<12} {kib:8.1f} KiB = {kib / 1024:.2f} MiB")
    pct = kib / MAIN_PACKAGE_LIMIT_KIB * 100
    mark = "⚠️" if pct > 80 else "✅"
    print(f"  {mark} 占微信主包上限 4 MiB 的 {pct:.1f}%")
    engine = sum(f.stat().st_size for f in (BUILD_DIR / "cocos-js").rglob("*") if f.is_file()) / 1024
    print(f"     其中引擎 cocos-js {engine:.1f} KiB（占 {engine / MAIN_PACKAGE_LIMIT_KIB * 100:.1f}%）"
          f"——引擎计入主包，结案依据见 平台事实核对 §1.1.1 ✅ 块")


def open_devtools(cli: Path) -> None:
    print("\n▶ 打开微信开发者工具")
    subprocess.run([str(cli), "open", "--project", str(BUILD_DIR), "--lang", "zh"], check=False)


def main() -> int:
    parser = argparse.ArgumentParser(description="M04 构建入口")
    parser.add_argument("--open", action="store_true", help="构建完顺带打开微信开发者工具")
    parser.add_argument("--size-only", action="store_true", help="不构建，只量现有产物体积")
    args = parser.parse_args()

    if not START_SCENE.is_file():
        sys.exit(f"缺少起始场景 {START_SCENE.relative_to(ROOT)}。")

    if not args.size_only:
        cfg = read_client_config()
        write_build_config(cfg)
        code = build(resolve("CocosCreator", "COCOS_CREATOR", COCOS_CANDIDATES), start_scene_uuid())
        patch_appid(cfg)
    else:
        code = 0
    report_size()
    if args.open:
        open_devtools(resolve("微信开发者工具", "WX_DEVTOOLS_CLI", DEVTOOLS_CANDIDATES))
    return code


if __name__ == "__main__":
    sys.exit(main())
