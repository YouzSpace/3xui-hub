/**
 * 金额换算：后端一律「整数分」（bigint），前端只在展示与输入时换成人看的「元」。
 *
 * 为什么单独一个文件、而且全部用整数运算：
 *  - 分是整数，乘除 100 用 Math.floor / 字符串拼接可以**完全避开浮点误差**
 *    （0.1 + 0.2 那套在钱上会直接变成「多一分 / 少一分」的账单差异）。
 *  - 换算规则要被余额卡片、充值弹层、后台兑换码/调账三处共用，
 *    口径必须一处定义，否则同一个数字在不同页面显示成两个样子。
 */

/** 元输入：允许整数或 1–2 位小数，前后空白忽略；其它一律视为非法。 */
const YUAN_INPUT = /^(\d+)(?:\.(\d{1,2}))?$/

/**
 * 分 → 元字符串（固定两位小数，负数带前导减号）。
 * 1234 → '12.34'，5000 → '50.00'，-5 → '-0.05'
 */
export function centsToYuan(cents) {
  const n = Number.isFinite(Number(cents)) ? Math.trunc(Number(cents)) : 0
  const abs = Math.abs(n)
  const fraction = String(abs % 100).padStart(2, '0')
  return `${n < 0 ? '-' : ''}${Math.floor(abs / 100)}.${fraction}`
}

/** 分 → '¥12.34'（余额、价格统一加币种前缀）。 */
export function formatYuan(cents) {
  return `¥${centsToYuan(cents)}`
}

/**
 * 分 → 带符号的金额（流水用）：正数 '+¥5.00'，负数 '-¥5.00'。
 * 0 显示 '¥0.00'（不带符号，避免出现「+¥0.00」这种噪音）。
 */
export function formatSignedYuan(cents) {
  const n = Number.isFinite(Number(cents)) ? Math.trunc(Number(cents)) : 0
  if (n === 0) return formatYuan(0)
  return `${n > 0 ? '+' : '-'}${formatYuan(Math.abs(n))}`
}

/**
 * 元输入（字符串 / 数字）→ 分；非法返回 null（调用方据此拦下，不做兜底）。
 *
 * 只接受「正数 + 最多两位小数」，不接受负号：负数金额一律用「加/扣」这种
 * 显式方向表达（后台调账也走正的输入框 + 方向选择），避免用户把 '-' 打进金额里。
 */
export function yuanToCents(input) {
  const raw = String(input ?? '').trim()
  const matched = YUAN_INPUT.exec(raw)
  if (!matched) return null
  const yuan = Number(matched[1])
  const fraction = Number((matched[2] || '').padEnd(2, '0'))
  const cents = yuan * 100 + fraction
  // 超过 Number.MAX_SAFE_INTEGER 的金额已经不是「钱」了，直接判非法，
  // 免得把精度问题带进后端 bigint。
  if (!Number.isSafeInteger(cents)) return null
  return cents
}
