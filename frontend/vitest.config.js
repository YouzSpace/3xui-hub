import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': path.resolve(path.dirname(fileURLToPath(import.meta.url)), 'src'),
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    // 见 vitest.setup.js：Node 22+ 自带的 localStorage 全局会盖住 jsdom 的
    setupFiles: ['./vitest.setup.js'],
  },
})
