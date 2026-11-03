import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'
import path from 'node:path'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue()],
  build: {
    // 构建产物直接落到 server 的 embed 目录：server 用 go:embed 打包 dist，
    // 编译出的二进制自带前端，服务器上不用再单独放静态文件。
    // outDir 在项目根之外，vite 默认拒绝清空它（会残留上一版带 hash 的旧 chunk），
    // 所以必须显式 emptyOutDir。
    outDir: path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../server/internal/web/dist'),
    emptyOutDir: true,
  },
  resolve: {
    alias: {
      '@': path.resolve(path.dirname(fileURLToPath(import.meta.url)), 'src'),
    },
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        // 本地模拟节点的域名不可达，后端逐台探测要等超时，默认代理超时会 502
        timeout: 120000,
      },
      '/admin-api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        timeout: 120000,
      },
      '/storage': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
