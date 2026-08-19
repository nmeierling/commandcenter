import { useState } from 'react'
import { setToken } from '../api'

/**
 * Shown when the agent rejects our token. The agent prints a URL containing the
 * token at startup; pasting either the URL or the bare token works.
 */
export function TokenGate() {
  const [value, setValue] = useState('')

  const submit = (ev: React.FormEvent) => {
    ev.preventDefault()
    const token = extractToken(value)
    if (!token) return
    setToken(token)
    window.location.reload()
  }

  return (
    <div className="gate">
      <h1>This dashboard needs the agent's API token</h1>
      <p>
        The agent runs commands on your machine, so it will not accept requests
        without a token — otherwise any web page you happened to have open could
        start and stop your projects.
      </p>
      <p>Run this in a terminal and open the URL it prints:</p>
      <pre>devcc url</pre>
      <p>Or paste the URL (or just the token) here:</p>
      <form onSubmit={submit}>
        <label className="sr-only" htmlFor="token">
          API token
        </label>
        <input
          id="token"
          type="text"
          value={value}
          autoFocus
          spellCheck={false}
          placeholder="http://127.0.0.1:47123/?token=…"
          onChange={(e) => setValue(e.target.value)}
        />
        <button className="primary" type="submit" disabled={!extractToken(value)}>
          Connect
        </button>
      </form>
    </div>
  )
}

function extractToken(input: string): string | null {
  const raw = input.trim()
  if (!raw) return null
  try {
    const url = new URL(raw)
    const t = url.searchParams.get('token')
    if (t) return t
  } catch {
    // Not a URL; fall through to treating the input as a bare token.
  }
  return /^[a-f0-9]{32,}$/i.test(raw) ? raw : null
}
