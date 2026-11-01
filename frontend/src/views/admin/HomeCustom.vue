<script setup>
/**
 * 主页自定义 — 管理员用 HTML + CSS 自己搭公开首页。
 *
 * 页面本身是编辑器：左边写代码，右边实时预览，保存后公开首页立即生效。
 * 预览走的是和公开首页**同一个** CustomHome 组件，所以所见即所得，
 * 不存在「预览好看、上线不一样」。
 */
import { ref, computed, onMounted } from 'vue'
import { useToast } from '@/components/ui/Toast.vue'
import { adminApi } from '@/api/admin'
import CustomHome from '@/components/CustomHome.vue'
import Button from '@/components/ui/Button.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import Card from '@/components/ui/Card.vue'
import Badge from '@/components/ui/Badge.vue'

const toast = useToast()

const html = ref('')
const css = ref('')
const loading = ref(false)
const saving = ref(false)
const saved = ref(false)
const maxHtml = ref(1048576)
const maxCss = ref(262144)

// 面板上可插入的变量，和CustomHome.vue 的替换逻辑一一对应
const VARS = ['{{site_title}}', '{{site_subtitle}}', '{{year}}']

/**
 * 给 AI 的提示词模板。
 *
 * 为什么提供这个：管理员多半不会手写 HTML/CSS。给他们一段能直接粘给 AI 的
 * 需求描述，AI 产出代码后粘回上面两个框就能用，比空着两个文本框友好得多。
 * 里面写死的风格/技术约束都是本面板的真实情况：
 * - 字体与配色来自 tokens.css（--font-serif / --accent）
 * - 禁用标签是 CustomHome.vue 里 DOMPurify 的 FORBID_TAGS，写错会被静默剥掉
 * - CSS 作用域限制是 scopeCss()，写 body{} 会被改写而不是照原样生效
 * 三者任一写错，AI 给的代码在预览里就会缺一块，管理员还以为 AI 写错了。
 */
const AI_PROMPT = `帮我写一个静态网页首页，用于代理面板的公开首页（访客打开域名时看到的第一个页面）。

【输出格式】
只输出两个代码块，不要任何解释文字：
1. 第一个代码块标注 html
2. 第二个代码块标注 css

【页面要求】
- 单文件页面，HTML 和 CSS 分开给我，不要合并成style 标签写在 head 里
- 结构用语义化标签（header / section / footer / h1 / h2 / p / a / button）
- 不要用任何 JavaScript，不要用 <script>、<iframe>、<form>、<object>、<embed>，这些会被自动剥掉
- 不要引入外部 JS 框架或 CSS 框架（Tailwind、Bootstrap 等），页面只允许 HTML + 纯 CSS
- 图片只能用直链 URL，优先用 CSS渐变和纯色块代替图片

【风格要求】
- 浅色主题，整体克制、有质感，不要花哨渐变和重阴影
- 正文用系统无衬线字体：font-family: 'Inter', system-ui, -apple-system, 'Segoe UI', sans-serif
- 大标题想要编辑感可以用衬线体：font-family: 'Cormorant Garamond', Georgia, serif
- 面板主题色是近黑色 hsl(240 5.9% 10%)，按钮和强调文字用这个色系
- 留白要充足，字号层级分明，桌面端为主，移动端不能横向滚动
- 不要用 emoji 做图标

【可用变量】
在 HTML 里直接写下面的占位符，页面会自动替换成真实内容：
{{site_title}}站点名称
{{site_subtitle}}   站点副标题
{{year}}            当前年份

【CSS 限制】
- CSS 会自动加作用域前缀，只作用于首页内容区
- 不要写 body {}、html {}、:root {}，这些会被自动改写
- 不要用 position: fixed，不要用负数 z-index，会被降级
- 需要整页背景色的话，写在 .ch-custom-home 这个 class 上

【内容建议】
包含：顶部导航、主视觉（标题+一句话说明+行动按钮）、2-3 个卖点/优势介绍、数据或节点列表、页脚
文案用中文，不要 lorem ipsum 这类占位文字，写真实可信的说明。`

/**
 * 复制 AI 提示词。
 *
 * 不只靠 clipboard API：它在无用户手势、非安全上下文、被策略禁用时都会抛
 * NotAllowedError，而 execCommand('copy') 在部分浏览器静默返回 false。
 * 两条路都失败时至少把文本选中并滚到可视区，管理员按 Ctrl+C 就能拿到，
 * 不会卡在「复制失败也不知道为什么」。
 */
const copyPrompt = async () => {
  const fallbackSelect = () => {
    const el = document.querySelector('.hc-ai-preview')
    if (!el) return false
    // <pre> 里的文本要靠 Range 选中；先滚进视口，否则用户看不到选中了什么
    el.scrollIntoView({ block: 'center' })
    const range = document.createRange()
    range.selectNodeContents(el)
    const sel = window.getSelection()
    sel.removeAllRanges()
    sel.addRange(range)
    return true
  }

  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(AI_PROMPT)
      toast.success('提示词已复制，粘贴给 AI 后把返回的代码填进下面两个框')
      return
    }
  } catch (e) {
    // 落到下面的降级分支
  }

  try {
    const ta = document.createElement('textarea')
    ta.value = AI_PROMPT
    ta.style.position = 'fixed'
    ta.style.top = '-9999px'
    ta.setAttribute('readonly', '')
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    ta.remove()
    if (ok) {
      toast.success('提示词已复制，粘贴给 AI 后把返回的代码填进下面两个框')
      return
    }
  } catch (e) {
    // 继续降级
  }

  if (fallbackSelect()) {
    toast.error('浏览器阻止了自动复制，文本已选中，按 Ctrl+C 手动复制')
  } else {
    toast.error('复制失败，请手动选中提示词文本复制')
  }
}

const htmlSize = computed(() => new Blob([html.value]).size)
const cssSize = computed(() => new Blob([css.value]).size)

const fmtSize = (n) => {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

const overHtml = computed(() => htmlSize.value > maxHtml.value)
const overCss = computed(() => cssSize.value > maxCss.value)

const enabled = computed(() => html.value.trim() !== '')

const load = async () => {
  loading.value = true
  try {
    const res = await adminApi.getHomeCustom()
    if (res) {
      html.value = res.html || ''
      css.value = res.css || ''
      maxHtml.value = res.max_html || maxHtml.value
      maxCss.value = res.max_css || maxCss.value
    }
  } catch (e) {
    toast.error(e?.message || '加载失败')
  } finally {
    loading.value = false
  }
}

const save = async () => {
  if (overHtml.value) return toast.error(`HTML 超出上限（最多 ${fmtSize(maxHtml.value)}）`)
  if (overCss.value) return toast.error(`CSS 超出上限（最多 ${fmtSize(maxCss.value)}）`)
  saving.value = true
  try {
    await adminApi.saveHomeCustom({ html: html.value, css: css.value })
    toast.success('已保存，公开首页已生效')
    // 成功反馈：按钮短暂变对勾（Submit Morph 成功段）
    saved.value = true
    setTimeout(() => { saved.value = false }, 900)
  } catch (e) {
    toast.error(e?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

const reset = async () => {
  if (!confirm('清空自定义内容并恢复默认首页？此操作不可撤销。')) return
  saving.value = true
  try {
    await adminApi.resetHomeCustom()
    html.value = ''
    css.value = ''
    toast.success('已恢复默认首页')
  } catch (e) {
    toast.error(e?.message || '恢复失败')
  } finally {
    saving.value = false
  }
}

/** 点变量直接插到 HTML 光标处 */
const insertVar = (v) => {
  const el = document.getElementById('home-html-editor')
  if (!el) return
  const start = el.selectionStart ?? html.value.length
  const end = el.selectionEnd ?? html.value.length
  html.value = html.value.slice(0, start) + v + html.value.slice(end)
  requestAnimationFrame(() => {
    el.focus()
    el.setSelectionRange(start + v.length, start + v.length)
  })
}

const STARTER = `<div class="hero">
  <h1>{{site_title}}</h1>
  <p>用 HTML 和 CSS 自己搭这个页面</p>
  <a class="btn" href="/login">立即登录</a>
</div>`

const starterCss = `.hero {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 16px;
  text-align: center;
}
.btn {
  padding: 10px 24px;
  border-radius: 8px;
  background: #111;
  color: #fff;
  text-decoration: none;
}`

onMounted(load)
</script>

<template>
  <div class="hc">
    <div class="hc-head">
      <div>
        <h1 class="hc-title">主页</h1>
        <p class="hc-sub">
          用 HTML + CSS 自定义公开首页（访客打开域名时看到的页面）。
          <Badge :variant="enabled ? 'success' : 'info'">{{ enabled ? '自定义已启用' : '当前使用默认首页' }}</Badge>
        </p>
      </div>
      <div class="hc-actions">
        <Button variant="secondary" size="sm" :loading="saving" @click="reset">恢复默认</Button>
        <Button size="sm" :loading="saving" :success="saved" @click="save">保存并发布</Button>
      </div>
    </div>

    <div class="hc-grid">
      <!-- 左：编辑 -->
      <div class="hc-edit">
        <Card :padding="false">
          <div class="hc-pane">
            <div class="hc-pane-head">
              <span class="hc-pane-title">HTML</span>
              <span class="hc-size" :class="{ 'is-over': overHtml }">{{ fmtSize(htmlSize) }} / {{ fmtSize(maxHtml) }}</span>
            </div>
            <textarea
              id="home-html-editor"
              v-model="html"
              class="hc-code"
              rows="14"
              spellcheck="false"
              placeholder="在这里写 HTML…"
            ></textarea>
          </div>

          <div class="hc-pane">
            <div class="hc-pane-head">
              <span class="hc-pane-title">CSS</span>
              <span class="hc-size" :class="{ 'is-over': overCss }">{{ fmtSize(cssSize) }} / {{ fmtSize(maxCss) }}</span>
            </div>
            <textarea
              v-model="css"
              class="hc-code"
              rows="10"
              spellcheck="false"
              placeholder="样式写在这里，会自动套用到左侧内容上"
            ></textarea>
          </div>

          <div class="hc-pane hc-pane-vars">
            <span class="hc-pane-title">可插入变量</span>
            <div class="hc-vars">
              <button v-for="v in VARS" :key="v" type="button" class="hc-var" @click="insertVar(v)">{{ v }}</button>
            </div>
            <p class="hc-tip">
              安全提示：<code>&lt;script&gt;</code>、<code>&lt;iframe&gt;</code>、
              <code>&lt;form&gt;</code> 会被自动移除，事件属性也会被清掉。
              CSS 里写 <code>position: fixed</code> 会被降级为 <code>relative</code>，
              避免自定义内容盖住整个页面。
            </p>
          </div>

          <!--
            AI 提示词放在编辑区最下面：它是给不会写代码的人用的备用路径，
            不是主流程。放顶上会抢走 HTML/CSS 两个框的注意力。
          -->
          <div class="hc-pane hc-pane-ai">
            <div class="hc-pane-head">
              <span class="hc-pane-title">不会写？让 AI 帮你写</span>
              <CopyButton :text="AI_PROMPT" label="复制提示词" />
            </div>
            <p class="hc-ai-lead">
              点「复制提示词」→ 粘给任意 AI → 把它返回的
              <code>html</code> 和 <code>css</code> 两个代码块，分别粘进上面对应的框 → 右侧预览满意后点「保存并发布」。
            </p>
            <pre class="hc-ai-preview">{{ AI_PROMPT }}</pre>
          </div>
        </Card>
      </div>

      <!-- 右：预览 -->
      <div class="hc-preview">
        <Card :padding="false">
          <div class="hc-pane-head">
            <span class="hc-pane-title">实时预览</span>
            <span class="hc-size">公开首页实际效果</span>
          </div>
          <div class="hc-frame">
            <CustomHome v-if="enabled" :html="html" :css="css" style-id="ch-custom-home-preview-style" />
            <!-- 没内容时给个起步模板，而不是空白框让人不知道从哪写 -->
            <div v-else class="hc-starter">
              <p class="hc-starter-title">当前使用默认首页</p>
              <p class="hc-starter-sub">左侧 HTML 为空时，公开首页显示面板自带的默认内容。</p>
              <Button variant="secondary" size="sm" @click="html = STARTER; css = starterCss">
                填入起步模板
              </Button>
            </div>
          </div>
        </Card>
      </div>
    </div>
  </div>
</template>

<style scoped>
.hc { max-width: 1400px; }
.hc-head { display: flex; align-items: flex-start; justify-content: space-between; gap: var(--space-4); margin-bottom: var(--space-5); flex-wrap: wrap; }
.hc-title { margin: 0 0 4px; font-size: var(--text-xl); font-weight: var(--font-bold); }
.hc-sub { margin: 0; font-size: var(--text-sm); color: var(--text-secondary); display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
.hc-actions { display: flex; gap: var(--space-2); }

.hc-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--space-4); align-items: start; }
@media (max-width: 1100px) { .hc-grid { grid-template-columns: 1fr; } }

.hc-pane { padding: var(--space-4); border-bottom: 1px solid var(--border-subtle); }
.hc-pane:last-child { border-bottom: none; }
.hc-pane-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: var(--space-2); }
.hc-pane-title { font-size: var(--text-sm); font-weight: var(--font-medium); color: var(--text-primary); }
.hc-size { font-size: var(--text-xs); color: var(--text-muted); font-variant-numeric: tabular-nums; }
.hc-size.is-over { color: var(--danger); font-weight: var(--font-medium); }

.hc-code {
  width: 100%;
  padding: var(--space-3);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--bg-input);
  color: var(--text-primary);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: 1.6;
  resize: vertical;
}
.hc-code:focus { outline: none; border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-muted); }

.hc-pane-vars { display: flex; flex-direction: column; gap: var(--space-2); }

/* AI 提示词区：折叠式展示，展开才看得全文，避免一进页面就被长文本淹没 */
.hc-pane-ai { background: var(--bg-subtle, var(--bg-hover)); }
.hc-ai-lead { margin: 0 0 var(--space-2); font-size: var(--text-xs); color: var(--text-secondary); line-height: 1.7; }
.hc-ai-lead code {
  background: var(--bg-page);
  padding: 1px 5px;
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}
.hc-ai-preview {
  max-height: 210px;
  overflow-y: auto;
  margin: 0;
  padding: var(--space-3);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  background: var(--bg-page);
  color: var(--text-secondary);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  line-height: 1.75;
  white-space: pre-wrap;
  word-break: break-word;
}
.hc-vars { display: flex; gap: var(--space-2); flex-wrap: wrap; }
.hc-var {
  padding: 3px 10px;
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-full);
  background: var(--bg-input);
  color: var(--text-secondary);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  cursor: pointer;
  transition: border-color var(--duration-fast) var(--ease-out), color var(--duration-fast) var(--ease-out);
}
.hc-var:hover { border-color: var(--accent); color: var(--accent); }
.hc-tip { margin: 0; font-size: var(--text-xs); color: var(--text-muted); line-height: 1.7; }
.hc-tip code { background: var(--bg-hover); padding: 1px 5px; border-radius: var(--radius-sm); font-family: var(--font-mono); }

/* 预览区固定高度并独立滚动，避免长内容把整页撑开 */
.hc-frame { height: 620px; overflow-y: auto; border-top: 1px solid var(--border-subtle); }
.hc-starter { display: flex; flex-direction: column; align-items: center; justify-content: center; gap: var(--space-3); height: 100%; padding: var(--space-8); text-align: center; }
.hc-starter-title { margin: 0; font-size: var(--text-base); font-weight: var(--font-medium); color: var(--text-primary); }
.hc-starter-sub { margin: 0; font-size: var(--text-sm); color: var(--text-muted); }
</style>