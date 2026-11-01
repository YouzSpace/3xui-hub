/**
 * 管理后台配色注入。
 *
 * 把管理员配置的颜色写成 <style id="panel-colors-style">，作用域 body.admin-app
 * （类由 AdminLayout 挂上；用户端与公开首页没有这个类，颜色天然不外溢）。
 * 传空配置 = 移除注入，回到 tokens.css 出厂值。
 *
 * 后端已对颜色值做过 #RRGGBB 白名单校验，这里再滤一遍做防御：
 * 拼进 <style> 的文本绝不能有白名单外的内容。
 */

const STYLE_ID = 'panel-colors-style'
const HEX_RE = /^#[0-9a-fA-F]{6}$/

const decls = (colors) =>
  Object.entries(colors || {})
    .filter(([k, v]) => k.startsWith('--') && typeof v === 'string' && HEX_RE.test(v))
    .map(([k, v]) => `${k}:${v};`)
    .join('')

export function applyPanelColors(config) {
  if (typeof document === 'undefined') return
  clearPanelColors()
  const light = decls(config?.light)
  const dark = decls(config?.dark)
  // 两套都为空 = 全是出厂值，不必注入
  if (!light && !dark) return
  const css = [
    light ? `body.admin-app{${light}}` : '',
    dark ? `html.dark body.admin-app{${dark}}` : '',
  ]
    .filter(Boolean)
    .join('\n')
  const el = document.createElement('style')
  el.id = STYLE_ID
  el.textContent = css
  document.head.appendChild(el)
}

export function clearPanelColors() {
  if (typeof document === 'undefined') return
  document.getElementById(STYLE_ID)?.remove()
}

/** HSL(0-360, 0-100, 0-100) → #rrggbb（标准换算，输出统一小写） */
export function hslToHex(h, s, l) {
  s /= 100
  l /= 100
  const k = (n) => (n + h / 30) % 12
  const a = s * Math.min(l, 1 - l)
  const f = (n) => l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)))
  const to = (x) =>
    Math.round(255 * x)
      .toString(16)
      .padStart(2, '0')
  return `#${to(f(0))}${to(f(8))}${to(f(4))}`
}

/** #rrggbb → 色相 0-360；中性色（灰/黑白，饱和度 < 12%）返回 null（色环不显示标记） */
export function hexToHue(hex) {
  const m = /^#?([0-9a-f]{6})$/i.exec(String(hex || '').trim())
  if (!m) return null
  const n = parseInt(m[1], 16)
  const r = ((n >> 16) & 255) / 255
  const g = ((n >> 8) & 255) / 255
  const b = (n & 255) / 255
  const max = Math.max(r, g, b)
  const min = Math.min(r, g, b)
  const d = max - min
  const l = (max + min) / 2
  const s = d === 0 || l === 0 || l === 1 ? 0 : d / (1 - Math.abs(2 * l - 1))
  if (s < 0.12) return null
  let h
  if (max === r) h = ((g - b) / d) % 6
  else if (max === g) h = (b - r) / d + 2
  else h = (r - g) / d + 4
  return (Math.round(h * 60) + 360) % 360
}

/**
 * 色相 → 整套协调配色（亮/暗各 6 值，hex）。
 *
 * 色环与预设共用的生成器：饱和度/亮度在这里统一收口，
 * 保证任何色相都是一套「能看」的配色，不依赖管理员的手感。
 */
export function buildPalette(hue) {
  const h = ((Math.round(hue) % 360) + 360) % 360
  return {
    light: {
      '--accent': hslToHex(h, 72, 42),
      '--bg-base': hslToHex(h, 40, 99),
      '--bg-surface': hslToHex(h, 45, 98),
      '--text-primary': hslToHex(h, 25, 12),
      '--text-secondary': hslToHex(h, 15, 45),
      '--border-subtle': hslToHex(h, 25, 90),
    },
    dark: {
      '--accent': hslToHex(h, 80, 65),
      '--bg-base': hslToHex(h, 20, 7),
      '--bg-surface': hslToHex(h, 20, 9),
      '--text-primary': hslToHex(h, 15, 96),
      '--text-secondary': hslToHex(h, 10, 68),
      '--border-subtle': hslToHex(h, 15, 20),
    },
  }
}
