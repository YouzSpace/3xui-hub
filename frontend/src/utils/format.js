/**
 * 数值格式化（仪表盘节点监控用）。
 *
 * 原则：**拿不到的数就是拿不到** —— 缺失一律返回占位符 '—'，
 * 绝不兜底成 0，否则「没有数据」会被画成「占用 0%」。
 */

/** 是有限数值（不是 null / undefined / NaN / Infinity）。 */
export const finite = (value) => typeof value === 'number' && Number.isFinite(value)

/** 百分比（0–100）。分母缺失或 <= 0 返回 null。 */
export function percent(used, total) {
  if (!finite(used) || !finite(total) || total <= 0) return null
  return Math.max(0, Math.min(100, (used / total) * 100))
}

/** 字节数 → 人类可读（1024 进制）。 */
export function formatBytes(value) {
  if (!finite(value) || value < 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  // 单位索引必须**双向**钳制：0 < value < 1 时 log 为负、floor 后是 -1，
  // units[-1] 是 undefined，会拼出「589 undefined」（监控页曲线的亚字节速率
  // 曾实测触发）。钳到 0（B 级）即可，此时 scaled = value 走整数显示。
  const index = value === 0 ? 0 : Math.max(0, Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1))
  const scaled = value / 1024 ** index
  return `${scaled.toFixed(index === 0 ? 0 : scaled >= 100 ? 0 : 1)} ${units[index]}`
}

/** 速率（字节/秒）。 */
export function formatSpeed(value) {
  if (!finite(value) || value < 0) return '—'
  return `${formatBytes(value)}/s`
}

/** 百分比数值 → "42%"；缺失返回占位符。 */
export function formatPercent(value, digits = 0) {
  if (!finite(value)) return '—'
  return `${value.toFixed(digits)}%`
}

/** 相对时间："刚刚 / 12 秒前 / 3 分钟前 / 2 小时前 / 1 天前"。 */
export function relativeTime(iso, now = Date.now()) {
  if (!iso) return '—'
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return '—'
  const diff = Math.max(0, now - t)
  if (diff < 5000) return '刚刚'
  if (diff < 60000) return `${Math.floor(diff / 1000)} 秒前`
  if (diff < 3600000) return `${Math.floor(diff / 60000)} 分钟前`
  if (diff < 86400000) return `${Math.floor(diff / 3600000)} 小时前`
  return `${Math.floor(diff / 86400000)} 天前`
}

/** 资源占用分级：<70 正常 / 70–89 偏高 / ≥90 高占用 / 无数据。 */
export function usageLevel(value) {
  if (!finite(value) || value < 0) return 'unknown'
  if (value >= 90) return 'critical'
  if (value >= 70) return 'warn'
  return 'ok'
}

/** 运行时长（秒）→ "3 天 4 小时"。 */
export function formatUptime(seconds) {
  if (!finite(seconds) || seconds < 0) return '—'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${minutes} 分钟`
  return `${minutes} 分钟`
}

/** 一组数值的平均值；全缺失返回 null（不要算成 0）。 */
export function average(values) {
  const valid = values.filter(finite)
  if (!valid.length) return null
  return valid.reduce((sum, value) => sum + value, 0) / valid.length
}

/** 一组数值求和；全缺失返回 null（「没有节点上报」不能显示成 0 B/s）。 */
export function sumOrNull(values) {
  const valid = values.filter(finite)
  if (!valid.length) return null
  return valid.reduce((total, value) => total + value, 0)
}
