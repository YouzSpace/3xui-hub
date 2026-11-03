<script setup>
/**
 * 节点地区地球（Canvas 绘制）。
 *
 * - 配色全部读现有 token（--success / --text-muted / --border-subtle …），亮暗主题自动跟随；
 *   主题切换时通过 MutationObserver 监听 html class 变化重取配色。
 * - 地图点阵与地区中心来自 assets/earth.json（Natural Earth 公有领域数据）。
 * - 旋转与光点在页面隐藏、滚出视口、系统「减少动态效果」时自动停，省电。
 *
 * 只负责画和交互；地区列表由父组件渲染，点列表调 focusRegion()。
 */
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import earth from '@/assets/earth.json'
import { aggregateRegions, curvePoint, packetPhase, projectGlobe } from '@/utils/globe'

const props = defineProps({
  nodes: { type: Array, default: () => [] },
})

const emit = defineEmits(['regions'])

const canvas = ref(null)
const regions = ref([])

const ariaLabel = computed(
  () => `节点地区示意图，${regions.value.length} 个地区。左右方向键旋转，上下方向键调整角度。`,
)

const LAND = earth.land
const CENTERS = earth.centers
/** 飞线动画的「家」：所有光点最终都汇聚到中国（用户定稿：双向——别国→CN、CN→别国） */
const HOME_REGION = 'CN'

let ctx = null
let raf = 0
let lastFrame = 0
let visible = true
let longitude = 110
let latitude = 20
let drag = null
let drawn = []
let packet = null
let nextPacketAt = 0
let palette = null
let resizeObserver = null
let intersectionObserver = null
let themeObserver = null
let reduced = null

/* ── 配色：从 CSS 变量取，canvas 只吃逗号语法的 hsl ────────────── */

function readPalette() {
  const style = getComputedStyle(document.documentElement)
  const read = (name) => style.getPropertyValue(name).trim()

  return {
    sphereInner: read('--bg-elevated'),
    sphereMid: read('--bg-surface'),
    sphereEdge: read('--border-subtle'),
    graticule: read('--border-subtle'),
    land: read('--text-muted'),
    online: read('--success'),
    offline: read('--text-muted'),
    label: read('--text-secondary'),
  }
}

/** hsl(240 5.9% 10%) → hsla(240, 5.9%, 10%, alpha)。非 hsl 值原样返回。 */
function withAlpha(color, alpha) {
  const match = String(color).match(
    /^hsl\(\s*([\d.]+)\s+([\d.]+)%\s+([\d.]+)%\s*(?:\/\s*([\d.]+)\s*)?\)$/i,
  )
  if (!match) return color
  const a = alpha ?? (match[4] !== undefined ? Number(match[4]) : 1)

  return `hsla(${match[1]}, ${match[2]}%, ${match[3]}%, ${a})`
}

/* ── 绘制 ──────────────────────────────────────────────────── */

function project(lon, lat, cx, cy, radius) {
  const p = projectGlobe(lon, lat, longitude, latitude)

  return { x: cx + p.x * radius, y: cy - p.y * radius, z: p.z }
}

/** 画一条只显示正面（z > 0）的经纬线。 */
function strokeMeridian(iterate, cx, cy, radius) {
  ctx.beginPath()
  let pen = false
  iterate((lon, lat) => {
    const p = project(lon, lat, cx, cy, radius)
    if (p.z > 0) {
      pen ? ctx.lineTo(p.x, p.y) : ctx.moveTo(p.x, p.y)
      pen = true
    } else {
      pen = false
    }
  })
  ctx.stroke()
}

function draw(time = performance.now()) {
  const el = canvas.value
  if (!el || !ctx) return

  const rect = el.getBoundingClientRect()
  const width = rect.width
  const height = rect.height
  if (!width || !height) return

  const dpr = Math.min(window.devicePixelRatio || 1, 2)
  if (el.width !== Math.round(width * dpr) || el.height !== Math.round(height * dpr)) {
    el.width = Math.round(width * dpr)
    el.height = Math.round(height * dpr)
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
  ctx.clearRect(0, 0, width, height)

  if (!palette) palette = readPalette()
  const tone = palette

  const cx = width * 0.5
  const cy = height * 0.5
  const radius = Math.min(width * 0.43, height * 0.46)

  // 球体：用底色/边框色做柔光，亮暗主题自动跟随，不写死颜色
  const glow = ctx.createRadialGradient(cx - radius * 0.35, cy - radius * 0.45, radius * 0.05, cx, cy, radius * 1.13)
  glow.addColorStop(0, tone.sphereInner)
  glow.addColorStop(0.8, tone.sphereMid)
  glow.addColorStop(1, tone.sphereEdge)
  ctx.fillStyle = glow
  ctx.beginPath()
  ctx.arc(cx, cy, radius, 0, Math.PI * 2)
  ctx.fill()

  // 经纬网
  ctx.strokeStyle = withAlpha(tone.graticule, 0.55)
  ctx.lineWidth = 0.6
  for (let lat = -60; lat <= 60; lat += 30) {
    strokeMeridian((visit) => {
      for (let lon = -180; lon <= 180; lon += 3) visit(lon, lat)
    }, cx, cy, radius)
  }
  for (let lon = -180; lon < 180; lon += 30) {
    strokeMeridian((visit) => {
      for (let lat = -90; lat <= 90; lat += 3) visit(lon, lat)
    }, cx, cy, radius)
  }

  // 陆地：点阵，越靠正面越大越实（用深度 z 做透明度，形成球面感）。
  // 透明度下限从 0.18 提到 0.38：原来正面最实处也只有 ~0.63，亮色主题下点太浅看不清（用户反馈）。
  for (let i = 0; i < LAND.length; i += 1) {
    const p = project(LAND[i][0], LAND[i][1], cx, cy, radius)
    if (p.z <= 0) continue
    ctx.fillStyle = withAlpha(tone.land, 0.38 + p.z * 0.52)
    ctx.beginPath()
    ctx.arc(p.x, p.y, 0.7 + p.z * 0.6, 0, Math.PI * 2)
    ctx.fill()
  }

  // 地区标记
  drawn = []
  for (const region of regions.value) {
    const center = CENTERS[region.code]
    if (!center) continue
    const p = project(center[0], center[1], cx, cy, radius)
    if (p.z <= 0.02) continue

    drawn.push({ ...p, region })
    const online = region.online > 0
    const color = online ? tone.online : tone.offline

    // 在线地区的呼吸光圈（系统要求减少动效时不画）
    if (online && !reduced?.matches) {
      const pulse = (time % 2200) / 2200
      ctx.strokeStyle = withAlpha(color, (1 - pulse) * 0.45)
      ctx.lineWidth = 1.3
      ctx.beginPath()
      ctx.arc(p.x, p.y, 5 + pulse * 13, 0, Math.PI * 2)
      ctx.stroke()
    }

    ctx.fillStyle = color
    ctx.beginPath()
    ctx.arc(p.x, p.y, online ? 4.5 : 3, 0, Math.PI * 2)
    ctx.fill()
    // 描边用球面色：亮点在浅底上、暗底上都分得开
    ctx.strokeStyle = tone.sphereMid
    ctx.lineWidth = 1.5
    ctx.stroke()

    // 地区代码加一圈底色描边：纯文字压在点阵陆地上看不清
    ctx.font = '11px system-ui, -apple-system, "PingFang SC", sans-serif'
    ctx.lineWidth = 3
    ctx.strokeStyle = tone.sphereMid
    ctx.strokeText(region.code, p.x + 8, p.y - 7)
    ctx.fillStyle = tone.label
    ctx.fillText(region.code, p.x + 8, p.y - 7)
  }

  drawPacket(time, tone, cx, cy, radius)
}

/**
 * 在线地区与中国（内置参照点）之间飞一条示意光点（**不代表真实链路或流量**）。
 * CN 是**内置参照点**（用户定稿）：不算地区、不算节点、不显示标记，
 * 只是飞线的汇聚端。飞线一端永远是 CN，另一端是随机的在线地区；
 * 方向随机（CN→别国 / 别国→CN），看起来是「双向汇聚」。
 * CN 不依赖 nodes 表 —— 即使没有任何中国节点，飞线照画。
 */
function drawPacket(time, tone, cx, cy, radius) {
  // CN 参照点：按地理中心投影；转到地球背面（z<=0.02）时这一帧不画
  const homeCenter = CENTERS[HOME_REGION]
  const homePt = homeCenter ? project(homeCenter[0], homeCenter[1], cx, cy, radius) : null

  const peers = drawn.filter((item) => item.region.online > 0)
  if (!homePt || homePt.z <= 0.02 || peers.length < 1) {
    packet = null

    return
  }

  // 光点已存在时校验另一端是否仍在线（地区列表刷新后可能失效）
  if (packet && !peers.some((p) => p.region.code === packet.from || p.region.code === packet.to)) {
    packet = null
    nextPacketAt = time + 400
  }

  if (!packet && time >= nextPacketAt) {
    const other = peers[Math.floor(Math.random() * peers.length)].region.code
    // 一半出站（CN → 别国）、一半入站（别国 → CN）
    const outbound = Math.random() < 0.5
    packet = outbound
      ? { from: HOME_REGION, to: other, start: time }
      : { from: other, to: HOME_REGION, start: time }
  }

  if (!packet) return

  const phase = packetPhase(time - packet.start)
  if (phase.done) {
    packet = null
    nextPacketAt = time + 400

    return
  }

  // 另一端随地球转动，每帧重取它在屏幕上的位置
  const other = drawn.find((item) => item.region.code === packet.from || item.region.code === packet.to)
  if (!other) {
    packet = null

    return
  }
  const a = packet.from === HOME_REGION ? homePt : other
  const b = packet.to === HOME_REGION ? homePt : other

  const dx = b.x - a.x
  const dy = b.y - a.y
  const control = { x: (a.x + b.x) / 2 - dy * 0.22, y: (a.y + b.y) / 2 + dx * 0.22 }

  ctx.strokeStyle = withAlpha(tone.online, phase.opacity * 0.45)
  ctx.lineWidth = 1.3
  ctx.beginPath()
  ctx.moveTo(a.x, a.y)
  for (let i = 1; i <= 40; i += 1) {
    const p = curvePoint(a, b, control, (phase.progress * i) / 40)
    ctx.lineTo(p.x, p.y)
  }
  ctx.stroke()

  if (phase.progress < 1) {
    for (let tail = 5; tail >= 0; tail -= 1) {
      const t = phase.progress - tail * 0.015
      if (t < 0) continue
      const p = curvePoint(a, b, control, t)
      ctx.fillStyle = withAlpha(tone.online, 1 - tail * 0.15)
      ctx.beginPath()
      ctx.arc(p.x, p.y, tail ? 2 : 4, 0, Math.PI * 2)
      ctx.fill()
    }
  }
}

/* ── 动画循环 ──────────────────────────────────────────────── */

function tick(time) {
  raf = 0
  if (!visible || document.hidden) return

  // 自转常开：拖动时也照转（用户定稿），只有系统要求减少动效时才停
  if (!reduced?.matches) {
    // 帧间隔封顶 100ms：标签页刚回来时不要一次转一大圈
    longitude += (lastFrame ? Math.min(time - lastFrame, 100) : 0) * 0.006
  }
  lastFrame = time
  draw(time)

  // 系统要求减少动效、又没有可飞的在线地区时，画完这一帧就停，不空转
  const hasPeers = drawn.filter((item) => item.region.online > 0).length > 1
  if (!reduced?.matches || hasPeers) raf = requestAnimationFrame(tick)
}

function wake() {
  if (!raf) {
    lastFrame = 0
    raf = requestAnimationFrame(tick)
  }
}

/* ── 交互 ──────────────────────────────────────────────────── */

/** 把地球转到某个地区正面（父组件点地区列表时调）。自转照旧继续。 */
function focusRegion(code) {
  const center = CENTERS[code]
  if (!center) return
  longitude = center[0]
  latitude = center[1]
  draw()
  wake()
}

function hitTest(event) {
  const rect = canvas.value.getBoundingClientRect()
  const x = event.clientX - rect.left
  const y = event.clientY - rect.top

  return drawn.find((item) => Math.hypot(item.x - x, item.y - y) < 16) || null
}

function onPointerDown(event) {
  drag = { x: event.clientX, y: event.clientY, moved: false }
  canvas.value.setPointerCapture(event.pointerId)
}

function onPointerMove(event) {
  if (!drag) return
  const dx = event.clientX - drag.x
  const dy = event.clientY - drag.y
  if (Math.abs(dx) + Math.abs(dy) > 2) drag.moved = true
  longitude -= dx * 0.35
  latitude = Math.max(-85, Math.min(85, latitude + dy * 0.25))
  drag.x = event.clientX
  drag.y = event.clientY
  draw()
}

function onPointerUp(event) {
  // 没拖动 = 点击：点到地区就定位过去
  if (drag && !drag.moved) {
    const hit = hitTest(event)
    if (hit) focusRegion(hit.region.code)
  }
  drag = null
}

function onKeyDown(event) {
  const step = 10
  const map = { ArrowLeft: -step, ArrowRight: step, ArrowUp: 0, ArrowDown: 0 }
  if (!(event.key in map)) return
  event.preventDefault()
  longitude += map[event.key]
  if (event.key === 'ArrowUp') latitude = Math.min(85, latitude + step)
  if (event.key === 'ArrowDown') latitude = Math.max(-85, latitude - step)
  draw()
  wake()
}

/* ── 数据 / 生命周期 ───────────────────────────────────────── */

function refresh() {
  regions.value = aggregateRegions(props.nodes)
  emit('regions', regions.value)
  draw()
  wake()
}

function onVisibilityChange() {
  if (!document.hidden) wake()
}

function onReducedChange() {
  draw()
  wake()
}

watch(() => props.nodes, refresh)

onMounted(() => {
  ctx = canvas.value.getContext('2d')
  reduced = window.matchMedia('(prefers-reduced-motion: reduce)')
  reduced.addEventListener('change', onReducedChange)

  resizeObserver = new ResizeObserver(() => {
    palette = null
    draw()
    wake()
  })
  resizeObserver.observe(canvas.value)

  intersectionObserver = new IntersectionObserver((entries) => {
    visible = entries[0].isIntersecting
    if (visible) wake()
  })
  intersectionObserver.observe(canvas.value)

  // 主题切换（html 的 class 变化）→ 重取配色重画
  themeObserver = new MutationObserver(() => {
    palette = null
    draw()
  })
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })

  document.addEventListener('visibilitychange', onVisibilityChange)
  refresh()
})

onBeforeUnmount(() => {
  if (raf) cancelAnimationFrame(raf)
  raf = 0
  resizeObserver?.disconnect()
  intersectionObserver?.disconnect()
  themeObserver?.disconnect()
  reduced?.removeEventListener('change', onReducedChange)
  document.removeEventListener('visibilitychange', onVisibilityChange)
})

defineExpose({ focusRegion })
</script>

<template>
  <div class="ng">
    <canvas
      ref="canvas"
      class="ng-canvas"
      tabindex="0"
      :aria-label="ariaLabel"
      @pointerdown="onPointerDown"
      @pointermove="onPointerMove"
      @pointerup="onPointerUp"
      @pointercancel="onPointerUp"
      @keydown="onKeyDown"
    />
  </div>
</template>

<style scoped>
.ng {
  display: flex;
  flex-direction: column;
  min-width: 0;
  flex: 1 1 auto;
  /* 卡片已收窄（右栏放「最近用户」），放开宽度上限让球尽量吃满可用空间 */
  max-width: 560px;
}
.ng-canvas {
  width: 100%;
  height: 340px;
  touch-action: pan-y;
  cursor: grab;
  outline-offset: 2px;
}
.ng-canvas:active {
  cursor: grabbing;
}
</style>
