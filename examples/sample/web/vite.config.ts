import { defineConfig } from 'vite'

export default defineConfig({
  server: {
    host: '0.0.0.0',
    port: 5173,
    // Accept the per-PR preview hostnames (pr-<n>.<domain>).
    allowedHosts: true,
    // Served behind suprcow + Caddy on https:443, so the HMR websocket connects
    // back over wss on 443. (For bare local `npm run dev`, drop this line.)
    hmr: { clientPort: 443 },
    // Local-dev parity: forward /api to the api container. Under suprcow,
    // same-origin routing already sends /api to the backend, so this is unused
    // in a preview — it only matters if you hit the dev server directly.
    proxy: { '/api': 'http://api:3000' },
  },
})
