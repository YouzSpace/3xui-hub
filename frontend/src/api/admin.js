import client from './client'

/**
 * Admin API（/admin/*，session cookie 鉴权）。
 */
export const adminApi = {
  login: (username, password, otp) => client.post('/admin-api/login', { username, password, otp }),
  logout: () => client.post('/admin-api/logout'),

  // 用户
  listUsers: () => client.get('/admin-api/users'),
  getUser: (id) => client.get(`/admin-api/users/${id}`),
  createUser: (data) => client.post('/admin-api/users', data),
  updateUser: (id, data) => client.put(`/admin-api/users/${id}`, data),
  deleteUser: (id) => client.delete(`/admin-api/users/${id}`),
  resetUserTraffic: (id) => client.post(`/admin-api/users/${id}/reset-traffic`),
  switchUserProtocol: (id, protocol) => client.post(`/admin-api/users/${id}/protocol`, { protocol }),
  renewUser: (id) => client.post(`/admin-api/users/${id}/renew`),

  // 节点
  listNodes: () => client.get('/admin-api/nodes'),
  getNode: (id) => client.get(`/admin-api/nodes/${id}`),
  createNode: (data) => client.post('/admin-api/nodes', data),
  updateNode: (id, data) => client.put(`/admin-api/nodes/${id}`, data),
  deleteNode: (id) => client.delete(`/admin-api/nodes/${id}`),
  testNode: (id) => client.post(`/admin-api/nodes/${id}/test`),
  probeInbounds: (data) => client.post('/admin-api/nodes/probe-inbounds', data),

  // 套餐
  listPlans: () => client.get('/admin-api/plans'),
  getPlan: (id) => client.get(`/admin-api/plans/${id}`),
  createPlan: (data) => client.post('/admin-api/plans', data),
  updatePlan: (id, data) => client.put(`/admin-api/plans/${id}`, data),
  deletePlan: (id) => client.delete(`/admin-api/plans/${id}`),

  // 设置
  getSettings: () => client.get('/admin-api/settings'),
  changePassword: (data) => client.post('/admin-api/change-password', data),
  updateUsername: (data) => client.post('/admin-api/update-username', data),

  // Google 2FA
  google2faGenerate: () => client.post('/admin-api/google2fa/generate'),
  google2faEnable: (otp) => client.post('/admin-api/google2fa/enable', { otp }),
  google2faDisable: (password) => client.post('/admin-api/google2fa/disable', { password }),

  // 支付配置
  listPayments: () => client.get('/admin-api/payments'),
  getPayment: (id) => client.get(`/admin-api/payments/${id}`),
  createPayment: (data) => client.post('/admin-api/payments', data),
  updatePayment: (id, data) => client.put(`/admin-api/payments/${id}`, data),
  deletePayment: (id) => client.delete(`/admin-api/payments/${id}`),

  // 订单
  listOrders: () => client.get('/admin-api/orders'),

  // 流量同步
  syncTraffic: () => client.post('/admin-api/sync-traffic'),

  // 站点信息
  getSiteSettings: () => client.get('/admin-api/site-settings'),
  updateSiteSettings: (data) => client.put('/admin-api/site-settings', data),

  // 备份
  backupExport: () => client.get('/admin-api/backup/export', { responseType: 'blob' }),
  backupPreview: (formData) => client.post('/admin-api/backup/preview', formData),
  backupImport: (mode, restoreEnv = true, siteUrl) => client.post('/admin-api/backup/import', { mode, restore_env: restoreEnv, site_url: siteUrl }),
}
