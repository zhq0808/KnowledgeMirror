import { defineConfig } from 'vite'
import path from 'path'
import { fileURLToPath } from 'url'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'

// ESM 下没有 __dirname，从 import.meta.url 推导
const __dirname = path.dirname(fileURLToPath(import.meta.url))

function figmaAssetResolver() {
  return {
    name: 'figma-asset-resolver',
    resolveId(id: string) {
      if (id.startsWith('figma:asset/')) {
        const filename = id.replace('figma:asset/', '')
        return path.resolve(__dirname, 'src/assets', filename)
      }
    },
  }
}

export default defineConfig({
  plugins: [
    figmaAssetResolver(),
    // The React and Tailwind plugins are both required for Make, even if
    // Tailwind is not being actively used – do not remove them
    react(),
    tailwindcss(),
  ],
  resolve: {
    alias: {
      // Alias @ to the src directory
      '@': path.resolve(__dirname, './src'),
    },
  },

  server: {
    // 绑 0.0.0.0：同时覆盖 127.0.0.1 和局域网 IP，手机可通过 http://<本机IP>:5173 访问。
    //
    // ⚠️ 但录音功能只在**安全来源**下可用：浏览器仅对 https、localhost、127.0.0.1
    // 挂载 navigator.mediaDevices，用 http://<局域网IP>:5173 打开时它是 undefined，
    // 点录音会直接报「浏览器只在安全来源下开放麦克风」。
    // 本机调试请一律走 http://localhost:5173；确实要用手机练语音时，
    // 给 dev server 配自签证书（@vitejs/plugin-basic-ssl）改走 https。
    host: '0.0.0.0',
    port: 5173,
    strictPort: true,
    // 开发时把后端请求透传到 Go 服务(8091)，前端直接调 /api、/health 即可，免跨域
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8091',
        ws: true,
      },
      '/health': 'http://127.0.0.1:8091',
    },
  },

  // File types to support raw imports. Never add .css, .tsx, or .ts files to this.
  assetsInclude: ['**/*.svg', '**/*.csv'],
})
