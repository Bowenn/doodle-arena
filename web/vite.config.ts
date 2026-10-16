/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// In the local stack the browser reaches Vite through the gateway (http://localhost:8080),
// so the hot-reload WebSocket must connect to the gateway's port as well.
const hmrClientPort = Number(process.env.VITE_HMR_CLIENT_PORT) || undefined

export default defineConfig({
  plugins: [react()],
  server: {
    host: true,
    port: 5173,
    strictPort: true,
    hmr: { clientPort: hmrClientPort },
    // "web" is the compose service name the gateway uses for /healthz/web.
    allowedHosts: ['localhost', 'web'],
  },
  test: {
    environment: 'jsdom',
  },
})
