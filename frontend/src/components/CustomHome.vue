<script setup>
/**
 * CustomHome.vue — 渲染管理员自定义的公开首页。
 *
 * 安全（这是本组件存在的核心原因）：
 * 用户填的 HTML 是**不可信输入**，直接 v-html 会有存储型 XSS。
 * 处理顺序：
 *   1. 模板变量替换（此时还是纯文本，不构成注入）
 *   2. DOMPurify 清洗：剥掉 <script>/<iframe>/<object>/事件属性/javascript: URL
 *   3. CSS 另做处理：不允许逃出容器（position:fixed / z-index 极高 / 影响 body 的选择器）
 *
 * 为什么不用 iframe 隔离：iframe 里没法用面板的主题变量，也不便做实时预览。
 * 沙箱 iframe（sandbox 属性）是更严格的方案，但会让管理员写的 CSS 拿不到
 * 字体与主题色，体验反而差。这里的取舍是：清洗 HTML + 约束 CSS 作用域。
 */
import { computed, watch, onMounted, onBeforeUnmount } from 'vue'
import DOMPurify from 'dompurify'
import { useSiteConfigStore } from '@/stores/siteConfig'

const props = defineProps({
  html: { type: String, default: '' },
  css: { type: String, default: '' },
  // 同页可能有多个实例（编辑页预览 + 公开首页），用 id 区分各自的 style 标签
  styleId: { type: String, default: 'ch-custom-home-style' },
})

const siteConfig = useSiteConfigStore()

/** 模板变量：管理员在 HTML 里写 {{site_title}} 就能拿到站点信息 */
const vars = computed(() => ({
  '{{site_title}}': siteConfig.title || 'ControlHub',
  '{{site_subtitle}}': siteConfig.subtitle || '',
  '{{year}}': String(new Date().getFullYear()),
}))

const replacedHtml = computed(() =>
  Object.entries(vars.value).reduce((acc, [k, v]) => acc.split(k).join(v), props.html || ''),
)

/** 清洗后的 HTML —— 真正渲染进 DOM 的东西 */
const safeHtml = computed(() =>
  DOMPurify.sanitize(replacedHtml.value, {
    // 明确禁掉：脚本执行容器与表单提交（防钓鱼表单冒提交登录页）
    FORBID_TAGS: ['script', 'iframe', 'object', 'embed', 'form', 'base', 'meta', 'link'],
    FORBID_ATTR: ['onerror', 'onload', 'onclick', 'onmouseover', 'onfocus', 'formaction'],
    ALLOW_DATA_ATTR: false,
  }),
)

/**
 * CSS 作用域化：把管理员写的选择器全部限制在 .ch-custom-home 容器内。
 *
 * 为什么必须做：<style> 注入的是 document.head，全局生效。
 * 早先只挡了 position:fixed 和负 z-index，管理员写 body{display:none}
 * 会让整个面板（含侧栏、所有输入框）空白，只能清空自定义内容才能救回来。
 * 对外部访客无影响（只有管理员能写这段 CSS），但管理员自己很容易误操作。
 *
 * 规则：
 * - 普通选择器（h1 / .hero / * / [attr]）→ 变成容器的后代
 * - 整页级（html / body / :root 及其组合）→ 映射到容器自身
 * - @media / @supports / @layer / @container → 递归处理内部规则
 * - @keyframes / @font-face 等 → 内部不是选择器，原样保留
 * - @import → 整条丢弃，它能拉外部样式表绕过全部限制
 */
const SCOPE = '.ch-custom-home'
const WHOLE_PAGE = /^(?:html|body|:root)(?:\s*[>+~]\s*(?:html|body|:root))*$/i
const stripPageRoots = (s) => s.replace(/^(?:(?:html|body|:root)\s*[>+~]?\s*)+/i, '')
/* 注释里可能有 } ，会骗过下面的花括号配对，先摘干净 */
const stripComments = (s) => s.replace(/\/\*[\s\S]*?\*\//g, '')

/** 逗号分隔的选择器列表拆分。:not(a,b) 里的逗号不算分隔符。 */
const splitSelectors = (list) => {
  const out = []
  let depth = 0, cur = '', quote = null
  for (let i = 0; i < list.length; i++) {
    const ch = list[i]
    if (quote) {
      cur += ch
      if (ch === quote && list[i - 1] !== '\\') quote = null
      continue
    }
    if (ch === '"' || ch === "'") { quote = ch; cur += ch; continue }
    if (ch === '\\') {
      // 转义序列整体吞掉：\, 表示名字里带逗号，不是选择器分隔符
      cur += ch
      if (i + 1 < list.length) { cur += list[i + 1]; i++ }
      continue
    }
    if (ch === '(' || ch === '[') depth++
    else if (ch === ')' || ch === ']') depth--
    if (ch === ',' && depth === 0) { out.push(cur); cur = '' } else cur += ch
  }
  if (cur.trim()) out.push(cur)
  return out
}

const scopeSelector = (sel) => {
  const s = sel.trim()
  if (!s) return null
  // 已经限定过就别再套一层
  if (s === SCOPE || s.startsWith(`${SCOPE} `) || s.startsWith(`${SCOPE}>`) || s.startsWith(`${SCOPE}:`)) return s
  if (WHOLE_PAGE.test(s)) return SCOPE
  return `${SCOPE} ${stripPageRoots(s)}`
}

/** 从 openIdx 处的 { 读到配对的 }，跳过字符串字面量（content: "}"） */
const readBlock = (src, openIdx) => {
  let depth = 0, quote = null
  for (let j = openIdx; j < src.length; j++) {
    const ch = src[j]
    if (quote) {
      if (ch === quote && src[j - 1] !== '\\') quote = null
      continue
    }
    if (ch === '"' || ch === "'") { quote = ch; continue }
    if (ch === '{') depth++
    else if (ch === '}') {
      depth--
      if (depth === 0) return { body: src.slice(openIdx + 1, j), next: j + 1 }
    }
  }
  return { body: src.slice(openIdx + 1), next: src.length }
}

/** 找下一个真实的 {，跳过注释和字符串；找不到返回 -1 */
const findBrace = (src, from) => {
  let quote = null
  for (let j = from; j < src.length; j++) {
    const ch = src[j]
    if (quote) {
      if (ch === quote && src[j - 1] !== '\\') quote = null
      continue
    }
    if (ch === '"' || ch === "'") { quote = ch; continue }
    // 注释里可能有 }，必须整段跳过
    if (ch === '/' && src[j + 1] === '*') {
      const e = src.indexOf('*/', j + 2)
      if (e === -1) return -1
      j = e + 1
      continue
    }
    if (ch === '{') return j
  }
  return -1
}

const scopeCss = (css) => {
  // 只摘注释（里面的 } 会骗过解析）和整条 @import/@charset。
  // 不能用正则全局删分号——data URI（data:image/png;base64）和
  // content:"a;b" 里的分号是内容的一部分，删了就把样式改坏了。
  const src = stripComments(String(css || ''))
    .replace(/@import[^;]*;?/gi, '')
    .replace(/@charset[^;]*;?/gi, '')

  let out = '', i = 0
  while (i < src.length) {
    const braceIdx = findBrace(src, i)
    if (braceIdx === -1) break
    const prelude = src.slice(i, braceIdx).trim()
    const { body, next } = readBlock(src, braceIdx)

    if (prelude.startsWith('@')) {
      const at = prelude.slice(1).split(/[\s(]/)[0].toLowerCase()
      // 条件类 at-rule 内部还是普通规则，递归处理；
      // keyframes/font-face 内部是帧选择器或描述，原样保留
      const nested = ['media', 'supports', 'layer', 'container'].includes(at)
      out += `${prelude}{${nested ? scopeCss(body) : body}}`
    } else if (prelude) {
      const sels = splitSelectors(prelude).map(scopeSelector).filter(Boolean)
      if (sels.length) out += `${sels.join(',')}{${body}}`
    }
    // 跳到 } 之后，把规则之间的分号/空白吃掉。
    // 这段间隙不属于任何选择器，前缀加在这里会变成「.ch-custom-home ;h2」，
    // 让上一条规则直接失效。
    i = next
    while (i < src.length && (src[i] === ';' || /\s/.test(src[i]))) i++
  }
  return out
}

const safeCss = computed(() =>
  scopeCss(props.css || '')
    // 容器内也不许盖住视口：fixed 会盖住整个窗口（含面板侧栏）
    .replace(/position\s*:\s*fixed\b/gi, 'position: relative')
    // 负 z-index 会钻到面板底层
    .replace(/z-index\s*:\s*-\d+/gi, 'z-index: 0'),
)

/**
 * 运行时注入 CSS。
 * 不能把 <style> 写在 template 里 —— Vue 编译期会忽略 <script>/<style> 这类
 * 有副作用的标签（dev 下直接报错「Tags with side effect are ignored」）。
 */
let styleEl = null

const applyCss = () => {
  if (!styleEl) {
    styleEl = document.createElement('style')
    styleEl.id = props.styleId
    document.head.appendChild(styleEl)
  }
  styleEl.textContent = safeCss.value
}

const removeCss = () => {
  styleEl?.remove()
  styleEl = null
}

// CSS 随输入实时更新（编辑页预览靠这个）
watch(safeCss, applyCss)
onMounted(applyCss)
onBeforeUnmount(removeCss)
</script>

<template>
  <div class="ch-custom-home">
    <!-- eslint-disable-next-line vue/no-v-html -- 上面已用 DOMPurify 清洗 -->
    <div v-html="safeHtml" />
  </div>
</template>

<style scoped>
.ch-custom-home {
  /* 自定义内容只在首页出现，不允许撑出横向滚动 */
  max-width: 100%;
  overflow-x: hidden;
}
.ch-custom-home > div {
  min-height: 100vh;
}
</style>