import { reactive, defineComponent, h, onMounted, onUnmounted } from 'vue'
import Modal from '@/components/ui/Modal.vue'
import Button from '@/components/ui/Button.vue'

/**
 * 统一确认弹窗。24 个页面各自在写 `confirm()` 或自己拼一个确认 Modal，
 * 前者在手机上是一串浏览器原生英文按钮、后者每个页面的按钮/文案/危险色都不一样。
 *
 *   const { confirm, ConfirmHost } = useConfirm()
 *   if (await confirm({ title: '删除用户', message: '确定删除？', danger: true })) { ... }
 *
 * 模板里必须挂一次 `<ConfirmHost />`（已挂在 App.vue，跟 `<Toast />` 同级），
 * 挂在 App 根部而不是每个页面里：一次挂载、全局可 await，页面卸载也不会把别人的弹窗带走。
 *
 * 几处刻意的处理：
 *
 *  1. **排队**：连续调两次 confirm（比如批量删除里两行各弹一次）不会串台 —— 第一个结论出来
 *     再显示第二个，两个 Promise 各自拿到自己的结果；后一个绝不会「继承」前一个的点击。
 *  2. **宿主卸载兜底**：ConfirmHost 卸载时把所有还挂着的调用按「取消」结掉，
 *     绝不让调用方的 await 永远悬着（挂着的 await 会卡住整条点击链路）。
 *  3. **没挂宿主时退回原生 confirm**：页面级单测、或哪个入口忘了套 App.vue 时，
 *     行为退化成迁移前的 `window.confirm`，而不是「点了按钮什么都没发生」。
 *     生产环境挂了 ConfirmHost 就永远走弹窗，这条只是兜底。
 */

const DEFAULTS = {
  title: '请确认',
  message: '',
  confirmText: '确定',
  cancelText: '取消',
  danger: false,
}

// 单例：整个应用只有一个确认弹窗，任何页面 confirm() 都投到这里
const state = reactive({ current: null })
const queue = []
let seq = 0
// 已挂载的宿主数量：0 = 没人渲染弹窗，只能退回原生 confirm
let hostCount = 0

const showHead = () => {
  state.current = queue.length ? queue[0].req : null
}

/** 只结掉「当前这一条」：弹窗在一次手势里触发多个事件（遮罩 + close）时不会误伤队列里下一条 */
const settle = (id, answer) => {
  const item = queue[0]
  if (!item || item.req.id !== id) return
  queue.shift()
  showHead()
  item.resolve(answer)
}

const nativeConfirm = (text) => {
  const fn = globalThis.confirm
  return typeof fn === 'function' ? !!fn(text) : false
}

/**
 * 弹一条确认。返回 Promise<boolean>：点确定 true，点取消 / 点遮罩 / 关闭 false。
 * 标题是可选装饰，原生兜底时把标题和正文拼成一行，信息不丢。
 */
export function confirm(options = {}) {
  const req = { id: ++seq, ...DEFAULTS, ...(options || {}) }

  if (!hostCount) {
    return Promise.resolve(nativeConfirm([req.title, req.message].filter(Boolean).join('\n')))
  }

  return new Promise((resolve) => {
    queue.push({ req, resolve })
    if (!state.current) showHead()
  })
}

const MESSAGE_STYLE =
  'margin: 0; color: var(--text-secondary); font-size: var(--text-sm); line-height: 1.6; white-space: pre-line;'

/**
 * 宿主组件：渲染那唯一一个弹窗。用渲染函数写在 composable 里，
 * 免得为一个 20 行的弹窗再拆一个 .vue 文件、再导入一遍 Modal/Button。
 */
export const ConfirmHost = defineComponent({
  name: 'ConfirmHost',
  setup() {
    onMounted(() => { hostCount++ })
    onUnmounted(() => {
      hostCount = Math.max(0, hostCount - 1)
      while (queue.length) queue.shift().resolve(false)
      state.current = null
    })

    return () => {
      const req = state.current
      return h(
        Modal,
        {
          modelValue: !!req,
          title: req?.title || DEFAULTS.title,
          width: '420px',
          // 确认框要压在页面弹窗之上：两者默认都是 1000，同层级下页面弹窗（DOM 更靠后）会盖住它。
          zIndex: 1400,
          'onUpdate:modelValue': (open) => {
            // 关闭态只可能来自用户操作（遮罩 / 右上角 X），按取消处理
            if (!open && state.current) settle(state.current.id, false)
          },
        },
        {
          default: () => h('p', { style: MESSAGE_STYLE }, req?.message || ''),
          footer: () => [
            h(
              Button,
              { variant: 'secondary', size: 'sm', onClick: () => req && settle(req.id, false) },
              () => req?.cancelText || DEFAULTS.cancelText,
            ),
            h(
              Button,
              {
                variant: req?.danger ? 'danger' : 'primary',
                size: 'sm',
                onClick: () => req && settle(req.id, true),
              },
              () => req?.confirmText || DEFAULTS.confirmText,
            ),
          ],
        },
      )
    }
  },
})

/**
 * 页面侧取用：只拿 confirm 就够了，ConfirmHost 已经在 App.vue 挂好。
 * 返回 ConfirmHost 是给「不想动 App.vue、想自己挂一个」的场景留的口子。
 */
export function useConfirm() {
  return { confirm, ConfirmHost }
}
