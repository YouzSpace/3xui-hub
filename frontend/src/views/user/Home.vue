<script setup>
/**
 * 公开主页 — Apple 风格 + 世界地图节点动画
 */
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { gsap } from 'gsap'
import { ScrollTrigger } from 'gsap/ScrollTrigger'
import Button from '@/components/ui/Button.vue'
import Icon from '@/components/ui/Icon.vue'
import ThemeToggle from '@/components/ui/ThemeToggle.vue'
import { useSiteConfigStore } from '@/stores/siteConfig'
import { userApi } from '@/api/user'

gsap.registerPlugin(ScrollTrigger)

const router = useRouter()
const siteConfig = useSiteConfigStore()
const plans = ref([])
const planTab = ref('total')
const container = ref(null)
let ctx

const filteredPlans = computed(() => plans.value.filter(p => p.type === planTab.value))

const fmtBytes = (b) => {
  if (!b) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(b) / Math.log(1024))
  return `${(b / Math.pow(1024, i)).toFixed(i ? 2 : 0)} ${u[i]}`
}

// 节点数据
const nodes = [
  { name: 'Tokyo', x: 82, y: 36 },
  { name: 'Seoul', x: 78, y: 34 },
  { name: 'Hong Kong', x: 74, y: 46 },
  { name: 'Singapore', x: 72, y: 56 },
  { name: 'Frankfurt', x: 50, y: 30 },
  { name: 'London', x: 47, y: 28 },
  { name: 'San José', x: 15, y: 36 },
  { name: 'Mumbai', x: 64, y: 46 },
]

onMounted(async () => {
  try { plans.value = await userApi.plans() } catch (e) {}
  if (!container.value) return

  ctx = gsap.context(() => {
    // === 响应式动画参数 ===
    const mm = gsap.matchMedia()
    mm.add('(max-width: 768px)', () => {
      // 移动端：缩短距离，减少位移
      ScrollTrigger.config({ ignoreMobileResize: true })
    })

    // === Nav 入场 ===
    gsap.fromTo('.lp-nav-inner', { y: -20, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.1 })

    // === Hero 入场 ===
    gsap.fromTo('.lp-hero-title', { y: 80, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 1.2, ease: 'power3.out', delay: 0.2 })
    gsap.fromTo('.lp-hero-sub', { y: 40, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.5 })
    gsap.fromTo('.lp-hero-actions', { y: 40, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.7 })
    gsap.fromTo('.lp-map-wrap', { scale: 0.9, autoAlpha: 0 }, { scale: 1, autoAlpha: 1, duration: 1.2, ease: 'power3.out', delay: 0.4 })

    // Hero 统计数据交错入场
    gsap.fromTo('.lp-hero-stat', { y: 30, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.6, ease: 'power3.out', stagger: 0.15, delay: 0.9 })

    // === 节点脉冲 ===
    gsap.utils.toArray('.lp-node-pulse').forEach((el, i) => {
      gsap.to(el, { scale: 3, opacity: 0, duration: 2, ease: 'power1.out', repeat: -1, delay: i * 0.3 })
    })

    // === 连接线流动 ===
    gsap.utils.toArray('.lp-map-line').forEach((line) => {
      const length = line.getTotalLength()
      gsap.set(line, { strokeDasharray: length, strokeDashoffset: length })
      gsap.to(line, { strokeDashoffset: 0, duration: 2, ease: 'power2.inOut', repeat: -1, repeatDelay: 1 })
    })

    // === Hero Pin ===
    gsap.to('.lp-hero-content', { scale: 0.85, y: -100, autoAlpha: 0, ease: 'power2.inOut', scrollTrigger: { trigger: '.lp-hero', start: 'top top', end: '+=500', scrub: true } })

    // === 功能区入场 ===
    // 大标题
    gsap.utils.toArray('.lp-feature-heading').forEach((el) => {
      gsap.fromTo(el, { x: -60, autoAlpha: 0 }, { x: 0, autoAlpha: 1, duration: 1, ease: 'power3.out', scrollTrigger: { trigger: el, start: 'top 85%', once: true } })
    })
    // 描述文字
    gsap.utils.toArray('.lp-feature-desc').forEach((el) => {
      gsap.fromTo(el, { y: 20, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', delay: 0.2, scrollTrigger: { trigger: el, start: 'top 85%', once: true } })
    })
    // 列表项交错
    gsap.utils.toArray('.lp-feature-list').forEach((list) => {
      const items = list.querySelectorAll('li')
      gsap.set(items, { autoAlpha: 0, x: -20 })
      ScrollTrigger.create({
        trigger: list,
        start: 'top 85%',
        once: true,
        onEnter: () => gsap.to(items, { autoAlpha: 1, x: 0, duration: 0.5, ease: 'power3.out', stagger: 0.1 })
      })
    })
    // 统计卡片
    gsap.utils.toArray('.lp-feature-card-outer').forEach((el) => {
      gsap.fromTo(el, { scale: 0.9, autoAlpha: 0 }, { scale: 1, autoAlpha: 1, duration: 0.8, ease: 'power3.out', scrollTrigger: { trigger: el, start: 'top 85%', once: true } })
    })
    // 功能区数字跳动
    gsap.utils.toArray('.lp-feature-stat-num').forEach((el) => {
      const text = el.textContent
      const numMatch = text.match(/(\d+)/)
      if (!numMatch) return
      const target = parseInt(numMatch[1], 10)
      const prefix = text.split(numMatch[1])[0]
      const suffix = text.split(numMatch[1])[1]
      const obj = { value: 0 }
      gsap.to(obj, { value: target, duration: 2, ease: 'power2.out', snap: { value: 1 },
        scrollTrigger: { trigger: el, start: 'top 85%', once: true },
        onUpdate() { el.textContent = prefix + Math.round(obj.value).toLocaleString() + suffix }
      })
    })
    // 分隔线展开
    gsap.utils.toArray('.lp-feature-divider').forEach((el) => {
      gsap.fromTo(el, { scaleX: 0, transformOrigin: 'left center' }, { scaleX: 1, duration: 1, ease: 'power2.inOut', scrollTrigger: { trigger: el, start: 'top 90%', once: true } })
    })

    // === Section 标题 ===
    gsap.utils.toArray('.lp-section-title, .lp-eyebrow:not(.lp-eyebrow-hero)').forEach((el) => {
      gsap.fromTo(el, { y: 30, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.8, ease: 'power3.out', scrollTrigger: { trigger: el, start: 'top 88%', once: true } })
    })

    // === 套餐 Tab ===
    gsap.fromTo('.lp-plan-tabs', { y: 20, autoAlpha: 0 }, { y: 0, autoAlpha: 1, duration: 0.6, ease: 'power3.out', scrollTrigger: { trigger: '.lp-plan-tabs', start: 'top 88%', once: true } })

    // === 套餐卡片 ===
    if (plans.value.length) {
      gsap.set('.lp-plan', { autoAlpha: 0, y: 60 })
      ScrollTrigger.batch('.lp-plan', { onEnter: (els) => gsap.to(els, { autoAlpha: 1, y: 0, duration: 0.8, ease: 'power3.out', stagger: 0.15 }), start: 'top 85%', once: true })
    }

    // === Hero 数字跳动 ===
    document.querySelectorAll('.lp-hero-stat .lp-stat-num').forEach((el) => {
      const text = el.getAttribute('data-target') || ''
      const numMatch = text.match(/(\d+)/)
      if (!numMatch) return
      const target = parseInt(numMatch[1], 10)
      const prefix = text.split(numMatch[1])[0]
      const suffix = text.split(numMatch[1])[1]
      const obj = { value: 0 }
      gsap.to(obj, { value: target, duration: 2, ease: 'power2.out', snap: { value: 1 },
        scrollTrigger: { trigger: el.closest('.lp-hero-stat') || el, start: 'top 80%', once: true },
        onUpdate() { el.textContent = prefix + Math.round(obj.value).toLocaleString() + suffix }
      })
    })

    // === CTA ===
    gsap.fromTo('.lp-cta-inner', { autoAlpha: 0, scale: 0.85, y: 40 }, { autoAlpha: 1, scale: 1, y: 0, duration: 1, ease: 'power3.out', scrollTrigger: { trigger: '.lp-cta', start: 'top 80%', once: true } })

    // === Footer ===
    gsap.fromTo('.lp-footer', { autoAlpha: 0 }, { autoAlpha: 1, duration: 0.6, scrollTrigger: { trigger: '.lp-footer', start: 'top 95%', once: true } })

  }, container.value)
})

onUnmounted(() => ctx?.revert())
</script>

<template>
  <div ref="container" class="lp">
    <!-- Nav -->
    <nav class="lp-nav">
      <div class="lp-nav-inner">
        <div class="lp-nav-brand">
          <span class="lp-nav-logo">
            <img v-if="siteConfig.logo" :src="siteConfig.logo" alt="" class="lp-nav-logo-img" />
            <Icon v-else name="shield" :size="20" />
          </span>
          <span class="lp-nav-name">{{ siteConfig.title }}</span>
        </div>
        <div class="lp-nav-links">
          <a href="#features">功能</a>
          <a href="#pricing">套餐</a>
        </div>
        <Button variant="primary" size="sm" class="lp-nav-btn" @click="router.push('/login')">登录</Button>
      </div>
    </nav>

    <!-- 主题切换 - 右上角固定 -->
    <div class="lp-theme-fixed">
      <ThemeToggle />
    </div>

    <!-- Hero + Map -->
    <section class="lp-hero">
      <div class="lp-hero-content">
        <span class="lp-eyebrow lp-eyebrow-hero">安全 · 高速 · 稳定</span>
        <h1 class="lp-hero-title">
          {{ siteConfig.title }}<span class="lp-hero-dot">.</span>
        </h1>
        <p v-if="siteConfig.subtitle" class="lp-hero-sub">{{ siteConfig.subtitle }}</p>
        <div class="lp-hero-actions">
          <button class="lp-btn lp-btn--primary" @click="router.push('/register')">
            立即开始
            <span class="lp-btn-icon"><Icon name="chevron-right" :size="16" /></span>
          </button>
          <button class="lp-btn lp-btn--ghost" @click="router.push('/login')">登录账户</button>
        </div>
        <div class="lp-hero-stats">
          <div class="lp-hero-stat">
            <span class="lp-stat-num" data-target="30+">0+</span>
            <span class="lp-stat-label">全球节点</span>
          </div>
          <div class="lp-hero-stat">
            <span class="lp-stat-num" data-target="5Gbps">0Gbps</span>
            <span class="lp-stat-label">单线带宽</span>
          </div>
          <div class="lp-hero-stat">
            <span class="lp-stat-num" data-target="99.9%">0%</span>
            <span class="lp-stat-label">在线率</span>
          </div>
        </div>
      </div>

      <!-- World Map -->
      <div class="lp-map-wrap">
        <img src="/images/world.svg" alt="" class="lp-map-img" />
        <svg class="lp-map-overlay" viewBox="0 0 100 100" preserveAspectRatio="none">
          <!-- 连接线 -->
          <line class="lp-map-line" x1="15" y1="36" x2="50" y2="30" />
          <line class="lp-map-line" x1="50" y1="30" x2="74" y2="46" />
          <line class="lp-map-line" x1="74" y1="46" x2="82" y2="36" />
          <line class="lp-map-line" x1="47" y1="28" x2="64" y2="46" />
          <line class="lp-map-line" x1="64" y1="46" x2="72" y2="56" />
          <line class="lp-map-line" x1="78" y1="34" x2="82" y2="36" />
          <line class="lp-map-line" x1="15" y1="36" x2="47" y2="28" />
        </svg>
        <!-- 节点 -->
        <div v-for="node in nodes" :key="node.name" class="lp-node" :style="{ left: node.x + '%', top: node.y + '%' }">
          <span class="lp-node-pulse"></span>
          <span class="lp-node-dot"></span>
          <span class="lp-node-label">{{ node.name }}</span>
        </div>
      </div>
    </section>

    <!-- Features -->
    <section id="features" class="lp-section">
      <span class="lp-eyebrow">核心优势</span>
      <h2 class="lp-section-title">大概你需要的就这些</h2>

      <div class="lp-feature-row">
        <div class="lp-feature-text">
          <h3 class="lp-feature-heading">无界<span class="lp-feature-dot">.</span></h3>
          <p class="lp-feature-desc">直连全球节点，无地域限制，低延迟高速访问</p>
          <ul class="lp-feature-list">
            <li>覆盖全球 30+ 节点</li>
            <li>智能路由，自动选择最优线路</li>
            <li>支持 IPLC 专线，延迟更低</li>
          </ul>
        </div>
        <div class="lp-feature-visual">
          <div class="lp-feature-card-outer">
            <div class="lp-feature-card-inner">
              <span class="lp-feature-stat-num">30+</span>
              <span class="lp-feature-stat-label">全球节点</span>
            </div>
          </div>
        </div>
      </div>

      <div class="lp-feature-divider"></div>

      <div class="lp-feature-row lp-feature-row--reverse">
        <div class="lp-feature-text">
          <h3 class="lp-feature-heading">加密<span class="lp-feature-dot">.</span></h3>
          <p class="lp-feature-desc">我们不追踪、不收集、不分享你的隐私数据</p>
          <ul class="lp-feature-list">
            <li>零日志策略，隐私有保障</li>
            <li>DNS 泄露防护</li>
            <li>先进加密协议，数据安全</li>
          </ul>
        </div>
        <div class="lp-feature-visual">
          <div class="lp-feature-card-outer">
            <div class="lp-feature-card-inner">
              <span class="lp-feature-stat-num">零</span>
              <span class="lp-feature-stat-label">日志策略</span>
            </div>
          </div>
        </div>
      </div>

      <div class="lp-feature-divider"></div>

      <div class="lp-feature-row">
        <div class="lp-feature-text">
          <h3 class="lp-feature-heading">优化<span class="lp-feature-dot">.</span></h3>
          <p class="lp-feature-desc">99.9% 在线率，7×24 运维保障，多设备同时在线</p>
          <ul class="lp-feature-list">
            <li>99.9% 服务可用性</li>
            <li>不限速，全节点可用</li>
            <li>手机、电脑、平板多端同时使用</li>
          </ul>
        </div>
        <div class="lp-feature-visual">
          <div class="lp-feature-card-outer">
            <div class="lp-feature-card-inner">
              <span class="lp-feature-stat-num">99.9%</span>
              <span class="lp-feature-stat-label">在线率</span>
            </div>
          </div>
        </div>
      </div>
    </section>

    <!-- Pricing -->
    <section id="pricing" class="lp-section lp-section--alt">
      <span class="lp-eyebrow">灵活方案</span>
      <h2 class="lp-section-title">选择适合你的套餐</h2>
      <div class="lp-plan-tabs">
        <button :class="['lp-plan-tab', { active: planTab === 'total' }]" @click="planTab = 'total'">总量套餐</button>
        <button :class="['lp-plan-tab', { active: planTab === 'period' }]" @click="planTab = 'period'">周期套餐</button>
      </div>
      <div class="lp-plans">
        <div v-for="(plan, idx) in filteredPlans" :key="plan.id" class="lp-plan" :class="{ 'lp-plan--highlight': idx === 1 }">
          <div class="lp-plan-outer">
            <div class="lp-plan-inner">
              <h3 class="lp-plan-name">{{ plan.name }}</h3>
              <div class="lp-plan-price">
                <span class="lp-plan-currency">¥</span>
                <span class="lp-plan-amount">{{ plan.price || 0 }}</span>
                <span class="lp-plan-unit">{{ plan.type === 'period' ? '/月' : '' }}</span>
              </div>
              <ul class="lp-plan-features">
                <template v-if="plan.type === 'period'">
                  <li>{{ plan.months }} 个月有效期</li>
                  <li>每月 {{ fmtBytes(plan.monthly_traffic) }}</li>
                  <li>到期自动续费</li>
                  <li>全节点可用</li>
                  <li>不限速</li>
                </template>
                <template v-else>
                  <li>总量 {{ fmtBytes(plan.total_traffic) }}</li>
                  <li>永久有效</li>
                  <li>用完为止</li>
                  <li>全节点可用</li>
                  <li>不限速</li>
                </template>
              </ul>
              <button class="lp-plan-btn" :class="idx === 1 ? 'lp-plan-btn--dark' : 'lp-plan-btn--light'" @click="router.push('/register')">
                选择 {{ plan.name }}
              </button>
            </div>
          </div>
        </div>
      </div>
      <p v-if="!filteredPlans.length" class="lp-empty">暂无可用套餐</p>
    </section>

    <!-- CTA -->
    <section class="lp-cta">
      <div class="lp-cta-inner">
        <h2>准备好开始了吗？</h2>
        <p>注册即可体验高速稳定的网络服务</p>
        <button class="lp-btn lp-btn--primary" @click="router.push('/register')">
          免费注册
          <span class="lp-btn-icon"><Icon name="chevron-right" :size="16" /></span>
        </button>
      </div>
    </section>

    <footer class="lp-footer">
      <p>{{ siteConfig.title }} &copy; {{ new Date().getFullYear() }}</p>
    </footer>
  </div>
</template>

<style scoped>
.lp {
  min-height: 100vh;
  background: var(--bg-base);
  color: var(--text-primary);
  font-family: -apple-system, SF Pro Display, PingFang SC, system-ui, sans-serif;
  -webkit-font-smoothing: antialiased;
  overflow-x: hidden;
}

/* Nav */
.lp-nav { position: fixed; top: 0; left: 0; right: 0; z-index: 100; display: flex; justify-content: center; padding: 16px 24px; backdrop-filter: saturate(180%) blur(20px); -webkit-backdrop-filter: saturate(180%) blur(20px); background: rgba(250,250,250,0.72); }
.lp-nav-inner { display: flex; align-items: center; gap: 32px; background: rgba(255,255,255,0.88); border: 1px solid var(--border-subtle); border-radius: 9999px; padding: 8px 8px 8px 20px; box-shadow: 0 2px 40px -8px rgba(0,0,0,0.06); max-width: 600px; width: 100%; }
.lp-nav-brand { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.lp-nav-logo { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; border-radius: 8px; background: var(--accent-muted); color: var(--accent); }
.lp-nav-logo-img { width: 28px; height: 28px; object-fit: contain; border-radius: 8px; }
.lp-nav-logo:has(img) { background: transparent; }
.lp-nav-name { font-weight: 600; font-size: 14px; letter-spacing: -0.01em; }
.lp-nav-links { display: flex; gap: 24px; flex: 1; }
.lp-nav-links a { color: var(--text-secondary); text-decoration: none; font-size: 13px; font-weight: 500; transition: color 0.3s; }
.lp-nav-links a:hover { color: var(--text-primary); }
.lp-nav-btn { flex-shrink: 0; border-radius: 9999px !important; }

/* 主题切换 - 右上角固定 */
.lp-theme-fixed {
  position: fixed;
  top: 20px;
  right: 24px;
  z-index: 101;
}

/* Eyebrow */
.lp-eyebrow { display: inline-block; font-size: 11px; font-weight: 600; letter-spacing: 0.08em; text-transform: uppercase; color: var(--accent); background: var(--accent-muted); padding: 6px 14px; border-radius: 9999px; margin-bottom: 16px; }

/* Hero */
.lp-hero { display: grid; grid-template-columns: 1fr 1fr; gap: 48px; align-items: center; max-width: 1200px; margin: 0 auto; padding: 140px 48px 100px; min-height: 100vh; }
.lp-hero-title { font-size: clamp(3rem, 6vw, 4.5rem); font-weight: 800; letter-spacing: -0.04em; line-height: 1.05; margin: 0 0 20px; }
.lp-hero-dot { color: var(--accent); }
.lp-hero-sub { font-size: 20px; color: var(--text-secondary); margin: 0 0 36px; font-weight: 500; line-height: 1.4; }
.lp-hero-actions { display: flex; gap: 12px; margin-bottom: 48px; }
.lp-hero-stats { display: flex; gap: 32px; }
.lp-hero-stat { display: flex; flex-direction: column; gap: 4px; }
.lp-stat-num { font-size: 1.8rem; font-weight: 800; letter-spacing: -0.03em; font-variant-numeric: tabular-nums; }
.lp-stat-label { font-size: 12px; color: var(--text-secondary); font-weight: 500; }

/* Map */
.lp-map-wrap { position: relative; display: flex; justify-content: center; align-items: center; }
.lp-map-img { width: 100%; max-width: 560px; opacity: 0.15; filter: grayscale(1); }
.lp-map-overlay { position: absolute; top: 0; left: 0; width: 100%; height: 100%; pointer-events: none; }
.lp-map-line { stroke: var(--accent); stroke-width: 0.15; opacity: 0.4; }
.lp-node { position: absolute; transform: translate(-50%, -50%); display: flex; flex-direction: column; align-items: center; }
.lp-node-dot { width: 8px; height: 8px; border-radius: 50%; background: var(--accent); display: block; }
.lp-node-pulse { position: absolute; width: 8px; height: 8px; border-radius: 50%; background: var(--accent); opacity: 0.6; }
.lp-node-label { font-size: 10px; color: var(--text-primary); font-weight: 600; white-space: nowrap; margin-top: 4px; }

/* Buttons */
.lp-btn { display: inline-flex; align-items: center; gap: 8px; padding: 14px 28px; border-radius: 9999px; font-size: 15px; font-weight: 600; border: none; cursor: pointer; transition: all 0.6s cubic-bezier(0.16, 1, 0.3, 1); font-family: inherit; letter-spacing: -0.01em; }
.lp-btn:active { transform: scale(0.98); }
.lp-btn--primary { background: var(--text-primary); color: var(--bg-surface); }
.lp-btn--primary:hover { background: #333; box-shadow: 0 8px 60px -12px rgba(0,0,0,0.20); }
.lp-btn--ghost { background: transparent; color: var(--text-primary); border: 1px solid rgba(0,0,0,0.12); }
.lp-btn--ghost:hover { background: rgba(0,0,0,0.03); }
.lp-btn--sm { padding: 12px 24px; font-size: 14px; }
.lp-btn-icon { display: inline-flex; align-items: center; justify-content: center; width: 22px; height: 22px; border-radius: 50%; background: rgba(255,255,255,0.2); transition: transform 0.3s cubic-bezier(0.16, 1, 0.3, 1); }
.lp-btn:hover .lp-btn-icon { transform: translateX(3px); }

/* Sections */
.lp-section { max-width: 1100px; margin: 0 auto; padding: 120px 48px; }
.lp-section--alt { background: var(--bg-surface); border-radius: 36px; max-width: 1060px; border: 1px solid var(--border-subtle); box-shadow: 0 2px 40px -8px rgba(0,0,0,0.06); }
.lp-section-title { font-size: clamp(2rem, 4vw, 3rem); font-weight: 800; letter-spacing: -0.03em; margin: 0 0 56px; line-height: 1.1; }

/* ========== Features — Row Layout ========== */
.lp-feature-row { display: grid; grid-template-columns: 1fr 1fr; gap: 60px; align-items: center; padding: 40px 0; }
.lp-feature-row--reverse { direction: rtl; }
.lp-feature-row--reverse > * { direction: ltr; }
.lp-feature-heading { font-size: clamp(2.5rem, 5vw, 4rem); font-weight: 800; letter-spacing: -0.04em; line-height: 1.05; margin: 0 0 16px; }
.lp-feature-dot { color: var(--accent); }
.lp-feature-desc { font-size: 16px; color: var(--text-secondary); margin: 0 0 24px; line-height: 1.6; }
.lp-feature-list { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 12px; }
.lp-feature-list li { font-size: 14px; color: var(--text-primary); font-weight: 500; padding-left: 20px; position: relative; }
.lp-feature-list li::before { content: ''; position: absolute; left: 0; top: 7px; width: 6px; height: 6px; border-radius: 50%; background: var(--accent); }
.lp-feature-visual { display: flex; justify-content: center; }
.lp-feature-card-outer { background: rgba(0,0,0,0.02); border: 1px solid var(--border-subtle); border-radius: 24px; padding: 6px; width: 100%; max-width: 280px; }
.lp-feature-card-inner { background: var(--bg-surface); border-radius: 18px; padding: 48px 32px; text-align: center; box-shadow: inset 0 1px 1px rgba(255,255,255,0.8); }
.lp-feature-stat-num { display: block; font-size: 3rem; font-weight: 800; letter-spacing: -0.03em; line-height: 1; margin-bottom: 8px; }
.lp-feature-stat-label { display: block; font-size: 13px; color: var(--text-secondary); font-weight: 500; }
.lp-feature-divider { height: 1px; background: var(--border-subtle); }

/* ========== Pricing — Kitty Style ========== */
.lp-plan-tabs { display: flex; justify-content: center; gap: 8px; margin-bottom: 48px; }
.lp-plan-tab { padding: 10px 24px; border-radius: 9999px; font-size: 14px; font-weight: 600; border: 1px solid rgba(0,0,0,0.08); background: transparent; color: var(--text-secondary); cursor: pointer; transition: all 0.4s cubic-bezier(0.16, 1, 0.3, 1); font-family: inherit; }
.lp-plan-tab:hover { color: var(--text-primary); border-color: rgba(0,0,0,0.15); }
.lp-plan-tab.active { background: var(--text-primary); color: var(--bg-surface); border-color: var(--text-primary); }
.lp-plans { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 20px; max-width: 780px; margin: 0 auto; align-items: stretch; }
.lp-plan { display: flex; }
.lp-plan-outer { flex: 1; border-radius: 24px; padding: 5px; transition: all 0.6s cubic-bezier(0.16, 1, 0.3, 1); }
.lp-plan:not(.lp-plan--highlight) .lp-plan-outer { background: var(--bg-surface); border: 1px solid var(--border-subtle); }
.lp-plan:not(.lp-plan--highlight) .lp-plan-outer:hover { box-shadow: 0 12px 60px -12px rgba(0,0,0,0.14); transform: translateY(-4px); }
.lp-plan--highlight .lp-plan-outer { background: var(--text-primary); border: 1px solid #333; box-shadow: 0 12px 60px -12px rgba(0,0,0,0.3); }
.lp-plan-inner { border-radius: 19px; padding: 36px 28px 28px; display: flex; flex-direction: column; gap: 16px; height: 100%; }
.lp-plan:not(.lp-plan--highlight) .lp-plan-inner { background: var(--bg-base); box-shadow: inset 0 1px 1px rgba(255,255,255,0.8); }
.lp-plan--highlight .lp-plan-inner { background: transparent; }
.lp-plan-name { font-size: 14px; font-weight: 700; margin: 0; letter-spacing: 0.05em; text-transform: uppercase; }
.lp-plan:not(.lp-plan--highlight) .lp-plan-name { color: var(--text-secondary); }
.lp-plan--highlight .lp-plan-name { color: var(--bg-surface); }
.lp-plan-price { display: flex; align-items: baseline; gap: 4px; margin: 4px 0 8px; }
.lp-plan-currency { font-size: 18px; font-weight: 700; }
.lp-plan:not(.lp-plan--highlight) .lp-plan-currency { color: var(--text-secondary); }
.lp-plan--highlight .lp-plan-currency { color: rgba(255,255,255,0.6); }
.lp-plan-amount { font-size: 3.5rem; font-weight: 800; letter-spacing: -0.04em; line-height: 1; font-variant-numeric: tabular-nums; }
.lp-plan:not(.lp-plan--highlight) .lp-plan-amount { color: var(--text-primary); }
.lp-plan--highlight .lp-plan-amount { color: var(--bg-surface); }
.lp-plan-unit { font-size: 14px; font-weight: 500; }
.lp-plan:not(.lp-plan--highlight) .lp-plan-unit { color: var(--text-secondary); }
.lp-plan--highlight .lp-plan-unit { color: rgba(255,255,255,0.5); }
.lp-plan-features { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 10px; flex: 1; padding-top: 16px; border-top: 1px solid; }
.lp-plan:not(.lp-plan--highlight) .lp-plan-features { border-color: var(--border-subtle); }
.lp-plan--highlight .lp-plan-features { border-color: rgba(255,255,255,0.1); }
.lp-plan-features li { font-size: 13px; font-weight: 500; padding-left: 16px; position: relative; }
.lp-plan-features li::before { content: ''; position: absolute; left: 0; top: 6px; width: 5px; height: 5px; border-radius: 50%; }
.lp-plan:not(.lp-plan--highlight) .lp-plan-features li { color: var(--text-primary); }
.lp-plan:not(.lp-plan--highlight) .lp-plan-features li::before { background: var(--text-primary); }
.lp-plan--highlight .lp-plan-features li { color: rgba(255,255,255,0.8); }
.lp-plan--highlight .lp-plan-features li::before { background: rgba(255,255,255,0.5); }
.lp-plan-btn { width: 100%; padding: 14px 24px; border-radius: 9999px; font-size: 14px; font-weight: 600; border: none; cursor: pointer; transition: all 0.4s cubic-bezier(0.16, 1, 0.3, 1); font-family: inherit; margin-top: auto; }
.lp-plan-btn:active { transform: scale(0.98); }
.lp-plan-btn--light { background: #f0f0f0; color: var(--text-primary); border: 1px solid rgba(0,0,0,0.08); }
.lp-plan-btn--light:hover { background: #e5e5e5; }
.lp-plan-btn--dark { background: var(--bg-surface); color: var(--text-primary); }
.lp-plan-btn--dark:hover { box-shadow: 0 4px 20px rgba(255,255,255,0.2); }
.lp-empty { text-align: center; color: var(--text-secondary); font-size: 14px; padding: 40px 0; }

/* CTA */
.lp-cta { text-align: center; padding: 120px 48px; max-width: 600px; margin: 0 auto; }
.lp-cta h2 { font-size: clamp(2rem, 4vw, 3rem); font-weight: 800; margin: 0 0 12px; letter-spacing: -0.03em; }
.lp-cta p { font-size: 17px; color: var(--text-secondary); margin: 0 0 32px; }

/* Footer */
.lp-footer { text-align: center; padding: 32px; font-size: 12px; color: var(--text-secondary); border-top: 1px solid var(--border-subtle); }

/* Mobile */
@media (max-width: 768px) {
  .lp-nav { padding: 12px 16px; }
  .lp-nav-inner { gap: 16px; padding: 6px 6px 6px 16px; justify-content: space-between; }
  .lp-nav-links { display: none; }
  .lp-theme-fixed { top: 14px; right: 60px; }
  .lp-hero { grid-template-columns: 1fr; gap: 40px; padding: 120px 24px 60px; text-align: center; min-height: auto; }
  .lp-hero-title { font-size: 2.5rem; }
  .lp-hero-actions { justify-content: center; }
  .lp-hero-stats { justify-content: center; }
  .lp-map-wrap { order: -1; }
  .lp-section { padding: 80px 24px; }
  .lp-section--alt { border-radius: 24px; margin: 0 16px; }
  .lp-feature-row { grid-template-columns: 1fr; gap: 32px; text-align: center; }
  .lp-feature-row--reverse { direction: ltr; }
  .lp-feature-list { align-items: center; }
  .lp-plans { grid-template-columns: 1fr; }
  .lp-cta { padding: 80px 24px; }
}

/* 暗色模式 */
html.dark .lp { background: #0a0a0a; color: #e5e5e5; }
html.dark .lp-nav { background: rgba(10,10,10,0.72); }
html.dark .lp-nav-inner { background: rgba(30,30,30,0.88); border-color: rgba(255,255,255,0.06); box-shadow: 0 2px 40px -8px rgba(0,0,0,0.3); }
html.dark .lp-nav-name { color: #fff; }
html.dark .lp-nav-links a { color: #86868b; }
html.dark .lp-nav-links a:hover { color: #e5e5e5; }
html.dark .lp-eyebrow { background: rgba(37,99,235,0.15); color: #60a5fa; }
html.dark .lp-hero-title { color: #fff; }
html.dark .lp-hero-sub { color: #86868b; }
html.dark .lp-stat-num { color: #fff; }
html.dark .lp-stat-label { color: #86868b; }
html.dark .lp-map-img { opacity: 0.08; filter: grayscale(1) invert(1); }
html.dark .lp-map-line { stroke: #60a5fa; }
html.dark .lp-node-dot { background: #60a5fa; }
html.dark .lp-node-pulse { background: #60a5fa; }
html.dark .lp-node-label { color: #e5e5e5; }
html.dark .lp-section-title { color: #fff; }
html.dark .lp-section--alt { background: rgba(20,20,20,0.8); border-color: rgba(255,255,255,0.06); box-shadow: 0 2px 40px -8px rgba(0,0,0,0.3); }
html.dark .lp-feature-card-outer { background: rgba(255,255,255,0.03); border-color: rgba(255,255,255,0.06); box-shadow: 0 2px 40px -8px rgba(0,0,0,0.3); }
html.dark .lp-feature-card-inner { background: rgba(20,20,20,0.8); box-shadow: inset 0 1px 1px rgba(255,255,255,0.05); }
html.dark .lp-feature-heading { color: #fff; }
html.dark .lp-feature-desc { color: #86868b; }
html.dark .lp-feature-list li { color: #e5e5e5; }
html.dark .lp-feature-list li::before { background: #60a5fa; }
html.dark .lp-feature-stat-num { color: #fff; }
html.dark .lp-feature-stat-label { color: #86868b; }
html.dark .lp-feature-divider { background: rgba(255,255,255,0.06); }
html.dark .lp-plan-tab { border-color: rgba(255,255,255,0.1); color: #86868b; }
html.dark .lp-plan-tab:hover { color: #e5e5e5; border-color: rgba(255,255,255,0.2); }
html.dark .lp-plan-tab.active { background: #fff; color: #0a0a0a; border-color: #fff; }
html.dark .lp-plan:not(.lp-plan--highlight) .lp-plan-outer { background: rgba(255,255,255,0.05); border-color: rgba(255,255,255,0.08); }
html.dark .lp-plan--highlight .lp-plan-outer { background: #fff; border-color: #fff; }
html.dark .lp-plan:not(.lp-plan--highlight) .lp-plan-inner { background: rgba(15,15,15,0.8); }
html.dark .lp-plan:not(.lp-plan--highlight) .lp-plan-name { color: #86868b; }
html.dark .lp-plan--highlight .lp-plan-name { color: #0a0a0a; }
html.dark .lp-plan:not(.lp-plan--highlight) .lp-plan-amount { color: #fff; }
html.dark .lp-plan--highlight .lp-plan-amount { color: #0a0a0a; }
html.dark .lp-plan:not(.lp-plan--highlight) .lp-plan-unit { color: #86868b; }
html.dark .lp-plan--highlight .lp-plan-unit { color: rgba(0,0,0,0.4); }
html.dark .lp-plan:not(.lp-plan--highlight) .lp-plan-features { border-color: rgba(255,255,255,0.06); }
html.dark .lp-plan:not(.lp-plan--highlight) .lp-plan-features li { color: #e5e5e5; }
html.dark .lp-plan--highlight .lp-plan-features li { color: rgba(0,0,0,0.7); }
html.dark .lp-plan-btn--light { background: rgba(255,255,255,0.08); color: #e5e5e5; border-color: rgba(255,255,255,0.1); }
html.dark .lp-plan-btn--dark { background: #0a0a0a; color: #fff; }
html.dark .lp-btn--ghost { color: #e5e5e5; border-color: rgba(255,255,255,0.2); }
html.dark .lp-cta h2 { color: #fff; }
html.dark .lp-cta p { color: #86868b; }
html.dark .lp-footer { border-color: rgba(255,255,255,0.06); }
</style>
