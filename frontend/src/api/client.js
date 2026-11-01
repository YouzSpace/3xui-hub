import axios from 'axios'

/**
 * ControlHub axios instance.
 * - baseURL: '/' (relies on vite server.proxy in dev, same-origin in prod)
 * - request interceptor: inject Bearer token from localStorage('access_token')
 * - response interceptor: unwrap { code, data }; code === 0 -> resolve data,
 *   otherwise reject with { message }.
 * - error: pull err.response.data.msg (fallback to err.message).
 */
const client = axios.create({
  baseURL: '/',
  // 弱网 / 跨境链路下 TLS 抖动会让单次请求偶发超过 15s（服务器侧实际是毫秒级响应），
  // 15s 全局超时容易误报「加载失败」（issue #34）；直连节点等更慢的接口仍单独放宽。
  timeout: 30000,
  withCredentials: true, // admin session cookie
  headers: {
    'Content-Type': 'application/json',
    Accept: 'application/json',
  },
})

client.interceptors.request.use(
  (config) => {
    const token = localStorage.getItem('access_token')
    if (token) {
      config.headers.Authorization = `Bearer ${token}`
    }
    // FormData 不设置 Content-Type，让浏览器自动处理
    if (config.data instanceof FormData) {
      delete config.headers['Content-Type']
    }
    return config
  },
  (error) => Promise.reject(error),
)

client.interceptors.response.use(
  (response) => {
    // Blob 响应（文件下载）直接返回
    if (response.config.responseType === 'blob') {
      return response.data
    }
    const payload = response.data
    // Non-JSON / raw responses (e.g. subscription text) — pass through.
    if (typeof payload !== 'object' || payload === null) {
      return payload
    }
    if (payload.code === 0) {
      // 分页列表（见 ApiResponse::successPage）：data 仍是当前页数组，
      // 顶层 pagination 元信息挂到数组上带出去 —— 老消费方按数组用不受影响，分页页读 .pagination
      if (payload.pagination !== undefined && Array.isArray(payload.data)) {
        return Object.assign(payload.data, { pagination: payload.pagination })
      }
      return payload.data
    }
    // Business error
    return Promise.reject({
      message: payload.msg || payload.message || `Error ${payload.code}`,
      code: payload.code,
      raw: payload,
    })
  },
  (error) => {
    // 幂等 GET 的弱网兜底（issue #34）：超时 / 网络层错误自动重试 1 次（退避 1.2s）。
    // 写操作不自动重试（避免重复提交）；被取消的请求不重试（用户已离开该页面）。
    const config = error.config || {}
    const isTimeout = error.code === 'ECONNABORTED' || /timeout/i.test(error.message || '')
    const isCanceled = error.code === 'ERR_CANCELED' || error.name === 'CanceledError'
    const isNetwork = !error.response && !isCanceled
    if (config.method === 'get' && !config._retried && (isTimeout || isNetwork)) {
      config._retried = true
      return new Promise((resolve) => setTimeout(resolve, 1200)).then(() => client(config))
    }
    const msg =
      (error.response && error.response.data && error.response.data.msg) ||
      (error.response && error.response.data && error.response.data.message) ||
      error.message ||
      'Network error'
    return Promise.reject({ message: msg, code: error.response && error.response.status, raw: error })
  },
)

export default client
