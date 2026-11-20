import { computed, inject } from 'vue'

/**
 * 入站页的共享上下文。
 *
 * 页面容器一次创建（表单 / 证书库 / Reality 检测 / 端口跳跃四块组合式函数），
 * 字段组件与三个弹窗从这里取用 —— 避免 20+ 个 prop 一层层往下传。
 */
export const INBOUND_CTX = Symbol('xray-inbound-ctx')

export function useInboundCtx() {
  const ctx = inject(INBOUND_CTX)
  if (!ctx) throw new Error('useInboundCtx() 只能在 XrayInbounds 内部使用')
  return ctx
}

/**
 * 把上下文里的 ref/computed 包成本组件内可直接 v-model 的别名。
 *
 * 为什么需要：模板编译只对「本地 ref」生成 `.value` 赋值，从 inject 拿到的引用
 * 在模板里直接 v-model 写回会丢掉更新（拿到的是 setup 返回对象的属性，不是 ref 本身）。
 */
export const writable = (source) =>
  computed({
    get: () => source.value,
    set: (v) => {
      source.value = v
    },
  })
