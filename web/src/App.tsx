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
  // 'all' is the unfiltered view, 'inbox' the projects nobody has filed yet,
  // and anything else is a category.
  const [tab, setTab] = useState('all')
  const [filter, setFilter] = useState<Filter>('all')
  const [toast, setToast] = useState<{ text: string; bad: boolean } | null>(null)
  const [rescanning, setRescanning] = useState(false)

  useEffect(() => {
    if (!toast) return
    const t = window.setTimeout(() => setToast(null), toast.bad ? 8000 : 3500)
    return () => window.clearTimeout(t)
  }, [toast])

  const notice = (text: string, bad = false) => setToast({ text, bad })

  // Every category in use, in a stable order. Derived from the projects rather
  // than from agent status so a tab appears the moment one is added, without
  // waiting for the next status poll.
  const categories = useMemo(() => {
    const seen = new Map<string, string>()
    for (const p of projects) {
      for (const c of p.categories ?? []) {
        if (!seen.has(c.toLowerCase())) seen.set(c.toLowerCase(), c)
      }
    }
    return [...seen.values()].sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()))
  }, [projects])

  const uncategorised = useMemo(
    () => projects.filter((p) => (p.categories ?? []).length === 0),
    [projects],
  )

  // A tab whose last project was filed — or whose category no longer exists —
  // would otherwise strand the view on an empty list with no way back.
  useEffect(() => {
    const gone =
      (tab === 'inbox' && uncategorised.length === 0) ||
      (tab !== 'all' &&
        tab !== 'inbox' &&
        !categories.some((c) => c.toLowerCase() === tab.toLowerCase()))
    if (gone) setTab('all')
  }, [categories, uncategorised.length, tab])

  const inTab = (p: Project) => {
    if (tab === 'all') return true
    if (tab === 'inbox') return (p.categories ?? []).length === 0
    return (p.categories ?? []).some((c) => c.toLowerCase() === tab.toLowerCase())
  }

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    return projects.filter((p) => {
      if (!inTab(p)) return false
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
  }, [projects, query, filter, tab, uncategorised.length])

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

      {categories.length > 0 && (
        <nav className="tabs" role="tablist" aria-label="Project categories">
          {/* Nothing to triage means no Inbox: an always-present empty tab is
              noise, and its disappearance is a useful signal in itself. */}
          {uncategorised.length > 0 && (
            <button
              role="tab"
              aria-selected={tab === 'inbox'}
              className={`tab${tab === 'inbox' ? ' active' : ''}`}
              onClick={() => setTab('inbox')}
              title="Projects that have no category yet"
            >
              Inbox<span className="tab-count">{uncategorised.length}</span>
            </button>
          )}
          <button
            role="tab"
            aria-selected={tab === 'all'}
            className={`tab${tab === 'all' ? ' active' : ''}`}
            onClick={() => setTab('all')}
          >
            All<span className="tab-count">{projects.length}</span>
          </button>
          {categories.map((c) => {
            const n = projects.filter((p) =>
              (p.categories ?? []).some((x) => x.toLowerCase() === c.toLowerCase()),
            ).length
            return (
              <button
                key={c}
                role="tab"
                aria-selected={tab === c}
                className={`tab${tab === c ? ' active' : ''}`}
                onClick={() => setTab(c)}
              >
                {c}<span className="tab-count">{n}</span>
              </button>
            )
          })}
        </nav>
      )}

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
          <div className="empty-state">
            {tab === 'all'
              ? 'No projects match this filter.'
              : tab === 'inbox'
                ? 'Nothing uncategorised matches this filter.'
                : `Nothing in “${tab}” matches this filter.`}
          </div>
        )}

        {visible.map((p) => (
          <ProjectCard
            key={p.id}
            project={p}
            canOpenBrowser={agent?.canOpenBrowser ?? true}
            dockerReady={agent?.daemonOk ?? true}
            knownCategories={categories}
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
