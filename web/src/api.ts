import type { AgentStatus, LogLine, Project } from './types'

const TOKEN_KEY = 'devcc.token'

/**
 * Token handling.
 *
 * In production the agent serves this app and the API from the same origin, and
 * the user arrives via the URL the agent printed: `/?token=…`. We stash the
 * token and strip it from the address bar so it does not linger in history or
 * get copy-pasted into a bug report.
 *
 * In development Vite proxies /api and injects the header itself, so there is
 * no token in the browser at all.
 */
export function bootstrapToken(): void {
  const url = new URL(window.location.href)
  const fromQuery = url.searchParams.get('token')
  if (fromQuery) {
    localStorage.setItem(TOKEN_KEY, fromQuery)
    url.searchParams.delete('token')
    window.history.replaceState({}, '', url.pathname + url.search + url.hash)
  }
}

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token.trim())
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
}

/** Thrown when the agent rejects our token, so the UI can show the gate. */
export class UnauthorizedError extends Error {
  constructor() {
    super('unauthorized')
  }
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly project?: Project,
  ) {
    super(message)
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  const token = getToken()
  if (token) headers.set('Authorization', `Bearer ${token}`)
  if (init.body) headers.set('Content-Type', 'application/json')

  let res: Response
  try {
    res = await fetch(path, { ...init, headers })
  } catch (err) {
    throw new ApiError(
      'Cannot reach the agent. Is `devcc` still running?',
      0,
    )
  }

  if (res.status === 401) throw new UnauthorizedError()

  const text = await res.text()
  const body = text ? safeParse(text) : {}

  if (!res.ok) {
    throw new ApiError(
      (body as { error?: string })?.error ?? `${res.status} ${res.statusText}`,
      res.status,
      (body as { project?: Project })?.project,
    )
  }
  return body as T
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return { error: text.slice(0, 400) }
  }
}

export const api = {
  status: () => request<AgentStatus>('/api/status'),

  projects: () =>
    request<{ projects: Project[] }>('/api/projects').then((r) => r.projects),

  rescan: () =>
    request<{ projects: Project[] }>('/api/projects/refresh', {
      method: 'POST',
    }).then((r) => r.projects),

  project: (id: string) => request<Project>(`/api/projects/${id}`),

  action: (id: string, action: 'start' | 'stop' | 'restart') =>
    request<Project>(`/api/projects/${id}/${action}`, { method: 'POST' }),

  open: (id: string, url?: string) =>
    request<{ opened: string }>(`/api/projects/${id}/open`, {
      method: 'POST',
      body: JSON.stringify({ url: url ?? '' }),
    }),

  resolvePort: (id: string, port: number, force: boolean) =>
    request<{ message: string; project: Project }>(
      `/api/projects/${id}/ports/${port}/resolve`,
      { method: 'POST', body: JSON.stringify({ force }) },
    ),

  logs: (id: string, tail = 300) =>
    request<{ lines: LogLine[]; source?: string; warning?: string }>(
      `/api/projects/${id}/logs?tail=${tail}`,
    ),
}

/**
 * Builds an EventSource URL. EventSource cannot set request headers, so the
 * token rides in the query string — over loopback only, to a server that also
 * verifies Origin and Host.
 */
export function streamUrl(path: string): string {
  const token = getToken()
  if (!token) return path
  const sep = path.includes('?') ? '&' : '?'
  return `${path}${sep}token=${encodeURIComponent(token)}`
}
