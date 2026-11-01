/**
 * 地球（正射投影）与地区工具 —— 纯计算，不碰 DOM。
 *
 * 绘制在 NodeGlobe.vue 里；这里只负责坐标、地区代码归一化和动画曲线。
 * 地图点阵 / 地区中心数据见 assets/earth.json（Natural Earth 公有领域数据）。
 */

/**
 * 正射投影：把经纬度投到以 (centerLon, centerLat) 为正面中心的单位球上。
 * 返回 { x, y, z }，x/y ∈ [-1, 1]；z > 0 表示在可见的正面，背面不绘制。
 */
export function projectGlobe(lon, lat, centerLon = 110, centerLat = 20) {
  const rad = Math.PI / 180
  const l = (lon - centerLon) * rad
  const p = lat * rad
  const c = centerLat * rad

  return {
    x: Math.cos(p) * Math.sin(l),
    y: Math.cos(c) * Math.sin(p) - Math.sin(c) * Math.cos(p) * Math.cos(l),
    z: Math.sin(c) * Math.sin(p) + Math.cos(c) * Math.cos(p) * Math.cos(l),
  }
}

/**
 * 地区标识归一化成两位 ISO 代码。
 * - 接受两位字母（大小写皆可）或一面旗帜 emoji（🇭🇰 → HK）
 * - UK 归一到 GB：地图数据用的是 ISO 的 GB，订阅那边的旗帜映射用的是 UK
 * 识别不出返回 null。
 */
export function regionCode(value) {
  const raw = String(value ?? '').trim()
  if (!raw) return null

  const chars = [...raw]
  if (chars.length === 2 && chars.every((c) => c.codePointAt(0) >= 0x1f1e6 && c.codePointAt(0) <= 0x1f1ff)) {
    return chars.map((c) => String.fromCharCode(c.codePointAt(0) - 0x1f1e6 + 65)).join('')
  }

  if (/^[a-z]{2}$/i.test(raw)) {
    const upper = raw.toUpperCase()
    return upper === 'UK' ? 'GB' : upper
  }

  return null
}

/**
 * 两位代码 → 旗帜 emoji（区域指示符组合）。
 * 先归一化：UK 必须转成 GB，否则会拼出 🇺🇰 这种不存在的旗帜。
 */
export function regionFlag(code) {
  const normalized = regionCode(code)
  if (!normalized) return ''

  return String.fromCodePoint(...[...normalized].map((c) => 0x1f1e6 + c.charCodeAt(0) - 65))
}

/** 浏览器内置的地区中文名（不额外拉数据、不联网）。 */
const regionNames = typeof Intl !== 'undefined' && typeof Intl.DisplayNames === 'function'
  ? new Intl.DisplayNames(['zh-CN'], { type: 'region' })
  : null

/** Intl 给港澳的是「中国香港特别行政区」这种全称，列表里放不下，用简称覆盖。 */
const SHORT_NAMES = { HK: '中国香港', MO: '中国澳门' }

export function regionName(code) {
  if (!code) return '未知地区'
  if (SHORT_NAMES[code]) return SHORT_NAMES[code]
  try {
    return regionNames?.of(code) || code
  } catch {
    return code
  }
}

/**
 * 按地区汇总节点：{ code, total, online }[]。
 * 在线数多的排前面，同分按总数、再按代码，保证每次渲染顺序稳定（列表不跳）。
 */
export function aggregateRegions(nodes) {
  const map = new Map()

  for (const node of nodes || []) {
    const code = regionCode(node?.region)
    if (!code) continue
    const item = map.get(code) || { code, total: 0, online: 0 }
    item.total += 1
    if (node.status === 'online') item.online += 1
    map.set(code, item)
  }

  return [...map.values()].sort(
    (a, b) => b.online - a.online || b.total - a.total || a.code.localeCompare(b.code),
  )
}

/** 二次贝塞尔取点（光点弧线用）。 */
export function curvePoint(a, b, c, t) {
  const u = 1 - t
  return {
    x: u * u * a.x + 2 * u * t * c.x + t * t * b.x,
    y: u * u * a.y + 2 * u * t * c.y + t * t * b.y,
  }
}

/**
 * 光点动画相位。
 * progress 0→1 走完弧线；opacity 在最后 fade 毫秒内淡出；done 表示这一趟结束。
 */
export function packetPhase(elapsed, { travel = 1800, fade = 500 } = {}) {
  return {
    progress: Math.max(0, Math.min(1, elapsed / travel)),
    opacity: Math.max(0, Math.min(1, (travel + fade - elapsed) / fade)),
    done: elapsed >= travel + fade,
  }
}
