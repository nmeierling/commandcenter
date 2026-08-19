import { useCallback, useEffect, useRef, useState } from 'react'
import { api, streamUrl, UnauthorizedError } from '../api'
import type { AgentStatus, Project } from '../types'

interface Snapshot {
  projects: Project[]
  agent: AgentStatus
}

/**
 * Keeps the project list in sync with the agent.
 *
 * The agent already refreshes every project on a timer and pushes changes, so
 * this hook subscribes rather than polls. Polling from the browser would make
 * the expensive parts (`docker compose ps`, port scans) run once per open tab.
 */
export function useProjects() {
  const [projects, setProjects] = useState<Project[]>([])
  const [agent, setAgent] = useState<AgentStatus | null>(null)
  const [connected, setConnected] = useState(false)
  const [unauthorized, setUnauthorized] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const retry = useRef(0)

  const upsert = useCallback((next: Project) => {
    setProjects((current) => {
      const i = current.findIndex((p) => p.id === next.id)
      if (i < 0) return [...current, next]
      const copy = current.slice()
      copy[i] = next
      return copy
    })
  }, [])

  useEffect(() => {
    let closed = false
    let source: EventSource | null = null
    let timer: number | undefined

    const connect = () => {
      if (closed) return
      source = new EventSource(streamUrl('/api/events'))

      source.addEventListener('open', () => {
        retry.current = 0
        setConnected(true)
        setUnauthorized(false)
      })

      source.addEventListener('snapshot', (ev) => {
        const data = JSON.parse((ev as MessageEvent).data) as Snapshot
        setProjects(data.projects ?? [])
        setAgent(data.agent ?? null)
        setLoaded(true)
      })

      source.addEventListener('project', (ev) => {
        const data = JSON.parse((ev as MessageEvent).data) as { project: Project }
        if (data.project) upsert(data.project)
      })

      source.addEventListener('projects', (ev) => {
        const data = JSON.parse((ev as MessageEvent).data) as { projects: Project[] }
        if (data.projects) setProjects(data.projects)
      })

      source.addEventListener('error', () => {
        setConnected(false)
        source?.close()
        if (closed) return
        // A closed stream may mean a rejected token; a plain fetch tells us
        // which, and drives the token gate instead of an endless retry loop.
        api
          .status()
          .then(() => scheduleReconnect())
          .catch((err) => {
            if (err instanceof UnauthorizedError) setUnauthorized(true)
            else scheduleReconnect()
          })
      })
    }

    const scheduleReconnect = () => {
      if (closed) return
      const delay = Math.min(1000 * 2 ** retry.current++, 15000)
      timer = window.setTimeout(connect, delay)
    }

    // Fetch once up front so the first paint does not wait on the stream.
    api
      .projects()
      .then((list) => {
        setProjects(list)
        setLoaded(true)
      })
      .catch((err) => {
        if (err instanceof UnauthorizedError) setUnauthorized(true)
        setLoaded(true)
      })
    api.status().then(setAgent).catch(() => undefined)

    connect()
    return () => {
      closed = true
      if (timer) window.clearTimeout(timer)
      source?.close()
    }
  }, [upsert])

  const rescan = useCallback(async () => {
    const list = await api.rescan()
    setProjects(list)
    api.status().then(setAgent).catch(() => undefined)
  }, [])

  return { projects, agent, connected, unauthorized, loaded, rescan, upsert }
}
