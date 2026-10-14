import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

export default defineConfig({
  // Concurrent isolated acceptance servers must not overwrite another Vite process's prebundle.
  cacheDir: process.env.VITE_CACHE_DIR || 'node_modules/.vite',
  plugins: [vue(), { name: 'local-management-ingress', configureServer(server) {
    server.middlewares.use((req, res, next) => {
      const address = req.socket.remoteAddress || '';
      let host = '';
      try { host = new URL(`http://${req.headers.host || ''}`).hostname; } catch { /* rejected below */ }
      if (!['localhost', '127.0.0.1', '[::1]'].includes(host)) { res.statusCode = 403; res.end('Invalid host'); return; }
      if (req.url?.startsWith('/api/') && !['127.0.0.1', '::1', '::ffff:127.0.0.1'].includes(address)) { res.statusCode = 403; res.end('Local access only'); return; }
      if (req.url?.startsWith('/py/admin/')) { res.statusCode = 403; res.end('Private endpoint'); return; }
      next();
    });
  } }],
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.GO_PROXY_TARGET || 'http://localhost:8080',
        changeOrigin: true
      },
      '/py': {
        target: process.env.PY_PROXY_TARGET || 'http://127.0.0.1:8090',
        changeOrigin: true,
        configure(proxy) {
          proxy.on('proxyReq', (upstream, request, response) => {
            if (!/\/local\/(?:.*\/)?(?:mosaic|tiles)\//.test(request.url || '')) return;
            // GET has already finished sending its request body when a user pans.
            // Forward response-side disconnects too, so Python can discard queued work.
            const cancel = () => { if (!response.writableEnded) upstream.destroy(); };
            response.once('close', cancel);
            upstream.once('close', () => response.off('close', cancel));
          });
        }
      }
    }
  }
});
