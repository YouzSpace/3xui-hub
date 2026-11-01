import { vi } from 'vitest'

/**
 * jsdom 里跑 canvas 组件的替身环境。
 *
 * jsdom 没有 2D context、没有 ResizeObserver / IntersectionObserver / matchMedia，
 * 而且 getBoundingClientRect 返回全 0（会让绘制提前 return）。这里统一补上，
 * 并把 context 的每次调用记进 calls，供断言「到底画了什么」。
 */

export const calls = []
const gradient = { addColorStop: () => {} }

const fakeContext = new Proxy(
  {},
  {
    get: (_target, prop) => {
      if (prop === 'createRadialGradient' || prop === 'createLinearGradient') return () => gradient
      if (prop === 'measureText') return () => ({ width: 10 })
      return (...args) => {
        calls.push({ prop, args })
        return undefined
      }
    },
    set: () => true,
  },
)

export const callNames = () => calls.map((c) => c.prop)
export const arcCount = () => callNames().filter((n) => n === 'arc').length
export const drawnTexts = () => calls.filter((c) => c.prop === 'fillText').map((c) => c.args[0])

export function installCanvasEnv() {
  calls.length = 0

  HTMLCanvasElement.prototype.getContext = vi.fn(() => fakeContext)
  HTMLCanvasElement.prototype.getBoundingClientRect = vi.fn(() => ({
    width: 400,
    height: 250,
    top: 0,
    left: 0,
    right: 400,
    bottom: 250,
    x: 0,
    y: 0,
  }))

  global.ResizeObserver = class {
    observe() {}
    disconnect() {}
  }
  global.IntersectionObserver = class {
    constructor(callback) {
      this.callback = callback
    }
    observe() {
      this.callback([{ isIntersecting: true }])
    }
    disconnect() {}
  }
  window.matchMedia = vi.fn(() => ({
    matches: false,
    addEventListener: () => {},
    removeEventListener: () => {},
  }))

  // 动画帧一律不排队：绘制在 refresh() 里是同步发生的，测试不需要真的跑动画循环。
  // 不定义的话 fake timers 切回真实时钟时会把它还原成 undefined，后续用例直接报错。
  global.requestAnimationFrame = () => 0
  global.cancelAnimationFrame = () => {}
}
