import { useState } from 'react'
import { api, ApiError } from '../api'
import type { Port, Project } from '../types'
import { LogPanel } from './LogPanel'

interface Props {
  project: Project
  canOpenBrowser: boolean
  /** False when this project's runtime cannot act right now (docker is down). */
  dockerReady: boolean
  onUpdate: (p: Project) => void
  onNotice: (message: string, bad?: boolean) => void
}

const KIND_LABEL: Record<string, string> = {
  compose: 'Docker Compose',
  node: 'Node',
  go: 'Go',
  rust: 'Rust',
  python: 'Python',
  custom: 'Project',
}

export function ProjectCard({
  project,
  canOpenBrowser,
  dockerReady,
  onUpdate,
  onNotice,
}: Props) {
  const [showLogs, setShowLogs] = useState(false)
  const [pending, setPending] = useState<string | null>(null)
  const st = project.state
  const busy = pending ?? st.busy ?? null
  const running = st.status === 'running' || st.status === 'unhealthy'
  const conflicts = st.ports.filter((p) => p.state === 'conflict')

  // A compose project cannot be acted on without a daemon, and a project with
  // no start command cannot be acted on at all.
  const blocked =
    project.runtime === 'none'
      ? 'No start command is configured for this project'
      : project.runtime === 'compose' && !dockerReady
        ? 'The Docker daemon is not running'
        : null

  const act = async (action: 'start' | 'stop' | 'restart') => {
    setPending(action)
    try {
      onUpdate(await api.action(project.id, action))
    } catch (err) {
      if (err instanceof ApiError) {
        onNotice(`${project.name}: ${err.message}`, true)
        if (err.project) onUpdate(err.project)
      } else {
        onNotice(String(err), true)
      }
    } finally {
      setPending(null)
    }
  }

  const open = async () => {
    try {
      const res = await api.open(project.id)
      onNotice(`Opened ${res.opened}`)
    } catch (err) {
      onNotice(err instanceof ApiError ? err.message : String(err), true)
    }
  }

  const resolve = async (port: Port) => {
    const who = describeOwner(port)
    if (!window.confirm(`Stop ${who} to free port ${port.port}?`)) return
    try {
      const res = await api.resolvePort(project.id, port.port, false)
      onNotice(res.message)
      onUpdate(res.project)
    } catch (err) {
      onNotice(err instanceof ApiError ? err.message : String(err), true)
    }
  }

  return (
    <article className={`card${conflicts.length ? ' is-conflict' : ''}`}>
      <div className="card-top">
        <span className={`dot ${st.status}`} aria-hidden="true" />
        <span className="card-name">{project.name}</span>
        <span className="card-kind">{KIND_LABEL[project.kind] ?? project.kind}</span>
        <span className="sr-only">status: {st.status}</span>
        <div className="card-ports">
          {st.ports.map((p) => (
            <span
              key={p.port}
              className={`port ${p.state}`}
              title={portTitle(p)}
            >
              :{p.port}
            </span>
          ))}
        </div>
      </div>

      <div className="card-detail">
        <span>{statusLine(project, conflicts.length > 0)}</span>
        <span className="card-path" title={project.path}>
          {shortenPath(project.path)}
        </span>
      </div>

      {st.error && (
        <div className="notice bad">
          <span className="grow">{st.error}</span>
        </div>
      )}

      {conflicts.map((p) => (
        <div className="notice bad" key={p.port}>
          <span className="grow">
            Port <strong>{p.port}</strong> is occupied by {describeOwner(p)}.
            {p.owner?.command && (
              <>
                {' '}
                <code>{truncate(p.owner.command, 90)}</code>
              </>
            )}
          </span>
          {p.owner ? (
            <button className="danger" onClick={() => resolve(p)}>
              Resolve
            </button>
          ) : null}
        </div>
      ))}

      {project.runtime === 'none' && (
        <div className="notice warn">
          <span className="grow">
            No start command detected. Add one under this project in your config
            file to control it from here:{' '}
            <code>start: npm run dev</code>
          </span>
        </div>
      )}

      {st.containers.length > 0 && (
        <div className="containers">
          {st.containers.map((c) => (
            <ContainerRow key={c.name} name={c.service || c.name} state={c.state} status={c.status} health={c.health} />
          ))}
        </div>
      )}

      <div className="card-actions">
        {running ? (
          <>
            <button
              onClick={() => act('restart')}
              disabled={!!busy || !!blocked}
              title={blocked ?? undefined}
            >
              {busy === 'restart' ? 'Restarting…' : 'Restart'}
            </button>
            <button
              onClick={() => act('stop')}
              disabled={!!busy || !!blocked}
              title={blocked ?? undefined}
            >
              {busy === 'stop' ? 'Stopping…' : 'Stop'}
            </button>
          </>
        ) : (
          <button
            className="primary"
            onClick={() => act('start')}
            disabled={!!busy || !!blocked}
            title={blocked ?? undefined}
          >
            {busy === 'start' ? 'Starting…' : 'Start'}
          </button>
        )}
        <button
          onClick={open}
          disabled={project.urls.length === 0 || !canOpenBrowser}
          title={
            project.urls.length === 0
              ? 'No URL known for this project'
              : !canOpenBrowser
                ? 'The agent has no way to open a browser on this machine'
                : project.urls[0]
          }
        >
          Open
        </button>
        <button className="ghost" onClick={() => setShowLogs((v) => !v)}>
          {showLogs ? 'Hide logs' : 'Logs'}
        </button>
      </div>

      {showLogs && <LogPanel project={project} onClose={() => setShowLogs(false)} />}
    </article>
  )
}

function ContainerRow({
  name,
  state,
  status,
  health,
}: {
  name: string
  state: string
  status?: string
  health?: string
}) {
  return (
    <>
      <span className={`dot ${state === 'running' ? 'running' : 'stopped'}`} />
      <span className="svc">{name}</span>
      <span>
        {status || state}
        {health ? ` · ${health}` : ''}
      </span>
    </>
  )
}

function statusLine(p: Project, hasConflictNotice: boolean): string {
  const st = p.state
  // The conflict notice below already spells this out; repeating it in the
  // summary line just makes the card noisier.
  if (hasConflictNotice && st.status === 'conflict') return 'Not running'
  if (st.detail) return st.detail
  switch (st.status) {
    case 'running':
      return 'Running'
    case 'stopped':
      return 'Stopped'
    case 'unknown':
      return 'Status unknown'
    default:
      return st.status
  }
}

function describeOwner(p: Port): string {
  const o = p.owner
  if (!o) return 'an unidentified process'
  if (o.composeProject) return `compose project “${o.composeProject}” (${o.container})`
  if (o.container) return `container ${o.container}`
  if (o.process) return `${o.process} (pid ${o.pid})`
  return `pid ${o.pid}`
}

function portTitle(p: Port): string {
  const from = p.source ? ` (from ${p.source})` : ''
  switch (p.state) {
    case 'owned':
      return `Port ${p.port} is served by this project${from}`
    case 'conflict':
      return `Port ${p.port} is taken by ${describeOwner(p)}${from}`
    case 'unknown-owner':
      return `Port ${p.port} is in use, owner unknown${from}`
    default:
      return `Port ${p.port} is free${from}`
  }
}

function shortenPath(path: string): string {
  const parts = path.split('/')
  return parts.length > 4 ? `…/${parts.slice(-3).join('/')}` : path
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s
}
