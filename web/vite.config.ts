import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// SPA serves under / so we keep base root-relative. `make web-build` copies
// dist/ into internal/webui/dist for the Go embed, so outDir must be emptied
// on every build: hashed chunks would otherwise pile up forever and ship
// inside the binary (including stale UI strings from old builds).
export default defineConfig({
  plugins: [vue()],
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    assetsDir: '.',
    rollupOptions: {
      output: {
        entryFileNames: 'app.[hash].js',
        chunkFileNames: 'chunk.[hash].js',
        assetFileNames: (info) => {
          // keep css alongside js under hashed names so the embed keeps tree intact
          if (info.name && info.name.endsWith('.css')) return 'app.[hash].css'
          return 'asset.[hash][extname]'
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      // dev proxy so the SPA can hit the Go API without CORS
      '/auth': 'http://127.0.0.1:9090',
      '/admin': 'http://127.0.0.1:9090',
      '/metrics': 'http://127.0.0.1:9090',
      '/healthz': 'http://127.0.0.1:9090',
    },
  },
})