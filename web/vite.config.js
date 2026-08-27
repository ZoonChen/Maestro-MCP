import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';

const loopbackHosts = new Set(['127.0.0.1', 'localhost', '[::1]', '::1']);

function localBackendOrigin(rawURL) {
  let parsed;
  try {
    parsed = new URL(rawURL);
  } catch {
    throw new Error('MAESTRO_DEV_BACKEND_URL must be a valid absolute URL');
  }
  if (!['http:', 'https:'].includes(parsed.protocol) || !loopbackHosts.has(parsed.hostname)) {
    throw new Error('MAESTRO_DEV_BACKEND_URL must target a loopback HTTP(S) origin');
  }
  return parsed.origin;
}

export default defineConfig(({ command }) => {
  const isDevelopmentServer = command === 'serve';
  const backendURL = process.env.MAESTRO_DEV_BACKEND_URL || '';
  const authToken = process.env.MAESTRO_DEV_AUTH_TOKEN || '';

  if (isDevelopmentServer && !backendURL) {
    throw new Error('MAESTRO_DEV_BACKEND_URL is required for the local development proxy');
  }
  if (isDevelopmentServer && !authToken) {
    throw new Error(
      'MAESTRO_DEV_AUTH_TOKEN is required for the authenticated local development proxy',
    );
  }
  // The dev proxy never falls back to an implicit origin: without an explicit
  // MAESTRO_DEV_BACKEND_URL the dev server refuses to start.
  const backendOrigin = isDevelopmentServer ? localBackendOrigin(backendURL) : null;

  const proxy = isDevelopmentServer
    ? {
        '/api': {
          target: backendOrigin,
          changeOrigin: false,
          ws: true,
          configure(proxyServer) {
            const authorize = (proxyRequest) => {
              proxyRequest.setHeader('Authorization', `Bearer ${authToken}`);
            };
            proxyServer.on('proxyReq', authorize);
            proxyServer.on('proxyReqWs', authorize);
          },
        },
      }
    : undefined;

  const readOnlyProxyGuard = {
    name: 'maestro-read-only-development-proxy',
    apply: 'serve',
    configureServer(server) {
      server.middlewares.use('/api', (request, response, next) => {
        if (request.method === 'GET' || request.method === 'HEAD') {
          next();
          return;
        }
        response.statusCode = 405;
        response.setHeader('Allow', 'GET, HEAD');
        response.setHeader('Content-Type', 'application/json; charset=utf-8');
        response.end(JSON.stringify({
          error: 'The development dashboard proxy is read-only',
          error_code: 'DEV_PROXY_WRITE_DISABLED',
        }));
      });
    },
  };

  return {
    plugins: [preact(), readOnlyProxyGuard],
    base: '/dashboard/',
    build: {
      outDir: 'dist',
      emptyOutDir: true,
    },
    server: {
      host: '127.0.0.1',
      port: 5173,
      strictPort: true,
      proxy,
    },
  };
});
