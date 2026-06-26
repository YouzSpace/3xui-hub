import client from './client'

/**
 * 用户端 API（/api/*，Bearer 鉴权）。
 */
export const userApi = {
  login: (token) => client.post('/api/login', { token }).then((r) => r),
  loginEmail: (email, password) => client.post('/api/login-email', { email, password }).then((r) => r),
  register: (email, password, captcha) => client.post('/api/register', { email, password, captcha }).then((r) => r),
  captchaUrl: () => `/api/captcha?t=${Date.now()}`,
  me: () => client.get('/api/me'),
  syncTraffic: () => client.post('/api/sync-traffic'),
  nodes: () => client.get('/api/nodes'),
  plans: () => client.get('/api/plans'),

  // 支付
  paymentMethods: () => client.get('/api/payment/methods'),
  createOrder: (planId, paymentConfigId) => client.post('/api/payment/create', { plan_id: planId, payment_config_id: paymentConfigId }).then((r) => r),
  orderStatus: (orderNo) => client.get('/api/payment/status', { params: { order_no: orderNo } }),
  myOrders: () => client.get('/api/payment/orders'),
  retryPay: (orderNo) => client.post('/api/payment/retry', { order_no: orderNo }).then((r) => r),

  // 网站信息（公开）
  siteConfig: () => client.get('/api/site-config'),
}
