// HUD 数字滚动补间。依据《美术圣经》第 848 行：金币数字滚动时长 ≤400ms、ease-out。
//
// 为什么不做成组件、也不从 label.string 反解数字：
//   · 客流槽显示的是「已到店/上限」这种复合串（如 "1/35"），从字符串里抠数字会得到 135；
//   · 所以滚动状态按 Label 存在 WeakMap 里，**首次出现直接赋值不滚动**——
//     启动那一帧从 0 滚到 1280 是错觉而非信息，验收第 3 条要的是"不跳变"，不是"每次都要动"。
//
// 中途来新目标（5 秒一轮，400ms 就滚完，正常不会撞上；但开店点击会打断节拍）：
// 先停掉上一条 tween，从**当前显示值**接着滚，因此既不会回跳也不会两条 tween 抢同一个 Label。
import { Label, Tween, tween } from "cc";

/** 《美术圣经》第 848 行的上限值；再长会让玩家觉得"钱是慢慢加的"而不是"服务端已经给了"。 */
const ROLL_DURATION = 0.4;

/** 每个 Label 各自的滚动中间态。WeakMap：Label 被销毁后不必手动清理。 */
const rolling = new WeakMap<Label, { value: number }>();

/**
 * 把 Label 的数字滚到目标值。
 * @param label 目标 Label
 * @param target 服务端给的权威值（不做任何推算）
 * @param format 显示格式，默认十进制整数；客流槽用它拼上分母
 */
export function rollNumber(label: Label, target: number, format: (n: number) => string = String): void {
    const state = rolling.get(label);
    if (!state) {
        rolling.set(label, { value: target });
        label.string = format(target);
        return;
    }
    if (state.value === target) {
        label.string = format(target);
        return;
    }
    Tween.stopAllByTarget(state);
    tween(state)
        .to(ROLL_DURATION, { value: target }, {
            easing: "quadOut",
            onUpdate: () => {
                label.string = format(Math.round(state.value));
            },
        })
        .call(() => {
            // 收尾按权威值落定，避免四舍五入停在 1291 这种中间态
            label.string = format(target);
        })
        .start();
}

/** 场景销毁时调用：停掉挂在中间态上的补间，防止回调打到已销毁的 Label。 */
export function stopRoll(label: Label): void {
    const state = rolling.get(label);
    if (state) Tween.stopAllByTarget(state);
}
