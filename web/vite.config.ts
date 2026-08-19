import { readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

/**
 * Reads the agent's API token from disk.
 *
 * In development the browser never sees the token: Vite proxies /api and adds
 * the Authorization header here, in Node. That keeps the dev setup identical to
 * production from the app's point of view, without weakening the agent's auth
 * for the sake of convenience.
 */
function agentToken(): string | null {
  const stateDir =
    process.env.DEVCC_STATE_DIR ??
    (process.platform === 'darwin'
      ? join(homedir(), 'Library', 'Caches', 'devcc')
      : join(process.env.XDG_CACHE_HOME ?? join(homedir(), '.cache'), 'devcc'))
  try {
    return readFileSync(join(stateDir, 'token'), 'utf8').trim()
  } catch {
    return null
  }
}

const AGENT = process.env.DEVCC_AGENT ?? 'http://127.0.0.1:47123'

export default defineConfig(() => {
  const token = agentToken()
  if (!token) {
    console.warn(
      '[devcc] no agent token found — start the agent first, or set DEVCC_STATE_DIR.',
    )
  }

  return {
    plugins: [react()],
    server: {
      port: 5173,
      strictPort: true,
      proxy: {
        '/api': {
          target: AGENT,
          changeOrigin: true,
          // SSE must not be buffered by the dev proxy.
          configure: (proxy) => {
            proxy.on('proxyReq', (proxyReq) => {
              if (token) proxyReq.setHeader('Authorization', `Bearer ${token}`)
              // The agent only accepts origins it knows; present its own.
              proxyReq.setHeader('Origin', AGENT)
            })
          },
        },
      },
    },
    build: {
      outDir: 'dist',
      emptyOutDir: true,
      // The agent embeds this output; keep it self-contained and small.
      assetsDir: 'assets',
      sourcemap: false,
    },
  }
})
