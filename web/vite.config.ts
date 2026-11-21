import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url))
    }
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8090',
        changeOrigin: true
      }
    }
  },
  build: {
    outDir: '../dist',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // 独立 chunk：hls.js 约 410KB、mpegts.js 约 240KB，都单独打包按需
        // 加载（只有真的点开直播页才会用到），不然会被扔进通用 vendor chunk
        // 跟着首屏一起加载；element-plus 图标独立
        manualChunks(id) {
          if (id.includes('hls.js')) return 'vendor-hls'
          if (id.includes('mpegts.js')) return 'vendor-mpegts'
          if (id.includes('@element-plus/icons-vue')) return 'vendor-ep-icons'
          if (id.includes('element-plus')) return 'vendor-ep'
          if (id.includes('node_modules')) return 'vendor'
        }
      }
    }
  }
})
