import { useEffect, useMemo, useState } from 'react'
import { ProjectCard } from './components/ProjectCard'
import { TokenGate } from './components/TokenGate'
import { useProjects } from './hooks/useProjects'
import type { Project, Status } from './types'

type Filter = 'all' | 'running' | 'stopped' | 'problems'

const PROBLEM_STATUSES: Status[] = ['conflict', 'error', 'unhealthy']

export function App() {
  const { projects, agent, connected, unauthorized, loaded, rescan, upsert } =
    useProjects()
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const [toast, setToast] = useState<{ text: string; bad: boolean } | null>(null)
  const [rescanning, setRescanning] = useState(false)

  useEffect(() => {
    if (!toast) return
    const t = window.setTimeout(() => setToast(null), toast.bad ? 8000 : 3500)
    return () => window.clearTimeout(t)
  }, [toast])

  const notice = (text: string, bad = false) => setToast({ text, bad })

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    return projects.filter((p) => {
      if (q && !`${p.name} ${p.path} ${p.kind}`.toLowerCase().includes(q)) {
        return false
      }
      switch (filter) {
        case 'running':
          return p.state.status === 'running' || p.state.status === 'unhealthy'
        case 'stopped':
          return p.state.status === 'stopped'
        case 'problems':
          return PROBLEM_STATUSES.includes(p.state.status)
        default:
          return true
      }
    })
  }, [projects, query, filter])

  if (unauthorized) return <TokenGate />

  const counts = summarize(projects)

  return (
    <div className="app">
      <header className="header">
        <h1>Development Command Center</h1>
        <span className="spacer" />
        <button
          onClick={async () => {
            setRescanning(true)
            try {
              await rescan()
              notice('Rescanned project directories')
            } catch (err) {
              notice(String(err), true)
            } finally {
              setRescanning(false)
            }
          }}
          disabled={rescanning}
        >
          {rescanning ? 'Scanning…' : 'Rescan'}
        </button>
      </header>

      <div className="subline">
        <span className="chip">
          <span className={`dot ${connected ? 'running' : 'stopped'}`} />
          {connected ? 'live' : 'reconnecting…'}
        </span>
        <span>
          {counts.running} running · {counts.stopped} stopped
          {counts.problems > 0 && ` · ${counts.problems} need attention`}
        </span>
        {agent && !agent.dockerOk && (
          <span className="chip warn" title={agent.dockerError}>
            docker compose not found
          </span>
        )}
        {agent && agent.dockerOk && !agent.daemonOk && (
          <span className="chip warn">docker daemon not running</span>
        )}
        {agent && !agent.portOwnersOk && (
          <span className="chip warn">port ownership unavailable</span>
        )}
        {agent && !agent.canOpenBrowser && (
          <span className="chip warn">no browser opener on this machine</span>
        )}
      </div>

      <div className="toolbar">
        <input
          type="search"
          placeholder="Filter projects…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          aria-label="Filter projects"
        />
        <select
          value={filter}
          onChange={(e) => setFilter(e.target.value as Filter)}
          aria-label="Status filter"
        >
          <option value="all">All</option>
          <option value="running">Running</option>
          <option value="stopped">Stopped</option>
          <option value="problems">Needs attention</option>
        </select>
      </div>

      {agent && agent.dockerOk && !agent.daemonOk && (
        <div className="banner">
          Docker Compose is installed but the daemon is not reachable, so
          compose projects show as <em>unknown</em> and cannot be started. Start
          Docker and the dashboard will pick it up within a few seconds.
        </div>
      )}

      <main className="cards">
        {!loaded && <div className="empty-state">Loading projects…</div>}

        {loaded && projects.length === 0 && (
          <div className="empty-state">
            <strong>No projects found.</strong>
            <br />
            The agent scanned{' '}
            {agent?.scanRoots?.length
              ? agent.scanRoots.join(', ')
              : 'no directories'}
            .<br />
            Add roots under <code>scan.roots</code> in{' '}
            <code>{agent?.configPath || 'your config file'}</code>, or run{' '}
            <code>devcc init</code> to create one.
          </div>
        )}

        {loaded && agent?.missingRoots?.length ? (
          <div className="banner bad">
            <strong>These scan roots do not exist and were skipped:</strong>
            <ul style={{ margin: '6px 0 0', paddingLeft: 20 }}>
              {agent.missingRoots.map((root) => (
                <li key={root}>
                  <code>{root}</code>
                </li>
              ))}
            </ul>
            Check <code>scan.roots</code> in{' '}
            <code>{agent.configPath || 'your config file'}</code>.
          </div>
        ) : null}

        {loaded && projects.length > 0 && visible.length === 0 && (
          <div className="empty-state">No projects match this filter.</div>
        )}

        {visible.map((p) => (
          <ProjectCard
            key={p.id}
            project={p}
            canOpenBrowser={agent?.canOpenBrowser ?? true}
            dockerReady={agent?.daemonOk ?? true}
            onUpdate={upsert}
            onNotice={notice}
          />
        ))}
      </main>

      {toast && (
        <div className={`toast${toast.bad ? ' bad' : ''}`} role="status">
          {toast.text}
        </div>
      )}
    </div>
  )
}

function summarize(projects: Project[]) {
  let running = 0
  let stopped = 0
  let problems = 0
  for (const p of projects) {
    if (PROBLEM_STATUSES.includes(p.state.status)) problems++
    else if (p.state.status === 'running') running++
    else if (p.state.status === 'stopped') stopped++
  }
  return { running, stopped, problems }
}
