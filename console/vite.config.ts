/// <reference types="vitest/config" />
import fs from 'node:fs'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

// dist 整体 gitignore，仅 .gitkeep 入库作为 go:embed 占位（见
// console/embed.go：新克隆 go build 需要至少一个可嵌入文件）。
// vite 构建会清空 dist（emptyDir 只豁免 .git），收尾补回占位文件。
function preserveDistGitkeep(): Plugin {
  return {
    name: 'preserve-dist-gitkeep',
    apply: 'build',
    closeBundle() {
      fs.writeFileSync(path.resolve(__dirname, 'dist', '.gitkeep'), '')
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), preserveDistGitkeep()],
  base: '/console/',
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['src/test.setup.ts'],
  },
  server: {
    // 代理 API 到本地 Go server（configs/config.yaml.template 的 server.http
    // 端口），保证 dev 下 /v1 与页面同源，HttpOnly 会话 cookie 正常工作。
    proxy: {
      '/v1': 'http://localhost:9080',
    },
  },
})
