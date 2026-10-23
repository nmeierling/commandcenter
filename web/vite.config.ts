import { readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

function stateDir(): string {
  return (
    process.env.DEVCC_STATE_DIR ??
    (process.platform === 'darwin'
      ? join(homedir(), 'Library', 'Caches', 'devcc')
      : join(process.env.XDG_CACHE_HOME ?? join(homedir(), '.cache'), 'devcc'))
  )
}

/**
 * Reads the agent's API token from disk.
 *
 * In development the browser never sees the token: Vite proxies /api and adds
 * the Authorization header here, in Node. That keeps the dev setup identical to
 * production from the app's point of view, without weakening the agent's auth
 * for the sake of convenience.
 *
 * Read per request, not once at startup. Vite and the agent start together, so
 * reading it once meant losing a race on a first run and proxying every call
 * unauthenticated for the rest of the session — which looks like a broken app,
 * not like a startup ordering problem.
 */
function agentToken(): string | null {
  try {
    return readFileSync(join(stateDir(), 'token'), 'utf8').trim()
  } catch {
    return null
  }
}

const AGENT = process.env.DEVCC_AGENT ?? 'http://127.0.0.1:47123'

export default defineConfig(() => {
  if (!agentToken()) {
    console.warn(
      `[devcc] no agent token in ${stateDir()} yet — it will be picked up as soon as the agent creates one.`,
    )
  }
  let warned = false

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
              const token = agentToken()
              if (token) {
                proxyReq.setHeader('Authorization', `Bearer ${token}`)
              } else if (!warned) {
                warned = true
                console.warn('[devcc] proxying without a token — is the agent running?')
              }
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
