/**
 * Vitest 环境补丁：补上 localStorage / sessionStorage。
 *
 * 背景：Node 22+ 自带 `localStorage` 全局，但它只在启动时带了 `--localstorage-file`
 * 才有值，否则是 undefined 并打一句 ExperimentalWarning。在 vitest 的 jsdom 环境里
 * `window === globalThis`，这个内置 getter 会把 jsdom 自己的实现盖掉 —— 于是测试里
 * 裸写 `localStorage.clear()` 抛「Cannot read properties of undefined」，
 * 表现成断言失败，实际是环境缺件。
 *
 * 与其跟两个运行时的实现细节较劲，这里直接给一个内存版 Storage：行为符合 Web Storage
 * 规范（值一律转字符串、key / clear / length 齐全），测试完全自洽。
 */

function createStorage() {
  const store = new Map()

  return {
    get length() {
      return store.size
    },
    key(index) {
      return [...store.keys()][index] ?? null
    },
    getItem(key) {
      const k = String(key)

      return store.has(k) ? store.get(k) : null
    },
    setItem(key, value) {
      store.set(String(key), String(value))
    },
    removeItem(key) {
      store.delete(String(key))
    },
    clear() {
      store.clear()
    },
  }
}

for (const key of ['localStorage', 'sessionStorage']) {
  if (!globalThis[key]) {
    Object.defineProperty(globalThis, key, {
      value: createStorage(),
      configurable: true,
      writable: true,
    })
  }
}
