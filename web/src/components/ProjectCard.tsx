import { useState } from 'react'
import { api, ApiError, type ActionOptions } from '../api'
import type { Port, PortConflict, Project, Unit } from '../types'
import { CategoryEditor } from './CategoryEditor'
import { LogPanel } from './LogPanel'
import { ResolveDialog } from './ResolveDialog'
import { UnitRow } from './UnitRow'

interface Props {
  project: Project
  canOpenBrowser: boolean
  /** False when this project's runtime cannot act right now (docker is down). */
  dockerReady: boolean
  /** Every category in use, offered as completions when editing. */
  knownCategories: string[]
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
  knownCategories,
  onUpdate,
  onNotice,
}: Props) {
  const [showLogs, setShowLogs] = useState(false)
  const [editingCategories, setEditingCategories] = useState(false)
  const [copied, setCopied] = useState(false)
  const [pending, setPending] = useState<string | null>(null)
  const [dialog, setDialog] = useState<{
    conflict: PortConflict
    run: (opts: ActionOptions) => Promise<void>
    substitute: (target: string) => Promise<void>
    reset: () => Promise<void>
    current?: { substituted: boolean; moved: boolean }
  } | null>(null)

  const st = project.state
  const busy = !!(pending ?? st.busy)
  const units = st.units
  const anyRunning = units.some((u) => u.running)

  // A compose project has several units, so the card keeps whole-project
  // controls. A supervised app is a single row, and duplicating its buttons on
  // the card would just raise the question of which one you pressed.
  const multiUnit = project.runtime === 'compose'
  const runtimeBlocked =
    project.runtime === 'compose' && !dockerReady ? 'The Docker daemon is not running' : null

  const call = async (fn: () => Promise<Project>, label: string) => {
    setPending(label)
    try {
      onUpdate(await fn())
      setDialog(null)
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.conflict) {
          // A blocked start is a question, not a failure.
          const c = err.conflict
          const u = { id: c.unitId ?? '' } as Unit
          setDialog({
            conflict: c,
            run: async (opts) => call(() => retryWith(c, opts), label),
            substitute: (target) => (c.unitId ? substitute(u, target) : Promise.resolve()),
            reset: () => (c.unitId ? resetUnit(u) : Promise.resolve()),
          })
        } else {
          onNotice(`${project.name}: ${err.message}`, true)
        }
        if (err.project) onUpdate(err.project)
      } else {
        onNotice(String(err), true)
      }
    } finally {
      setPending(null)
    }
  }

  // Re-runs whatever was blocked, carrying the user's decision.
  const retryWith = (conflict: PortConflict, opts: ActionOptions) =>
    conflict.unitId
      ? api.unitAction(project.id, conflict.unitId, 'start', opts)
      : api.action(project.id, anyRunning ? 'restart' : 'start', opts)

  const projectAction = (action: 'start' | 'stop' | 'restart', opts: ActionOptions = {}) =>
    call(() => api.action(project.id, action, opts), action)

  const unitAction = (unit: Unit, action: 'start' | 'kill', opts: ActionOptions = {}) =>
    call(() => api.unitAction(project.id, unit.id, action, opts), `${action}:${unit.id}`)

  const openDialog = (unit: Unit, conflict: PortConflict) =>
    setDialog({
      conflict,
      run: async (opts) => unitAction(unit, 'start', opts),
      substitute: (target) => substitute(unit, target),
      reset: () => resetUnit(unit),
      current: {
        substituted: !!unit.substitute,
        moved: unit.ports.some((p) => p.declared && p.declared !== p.configured),
      },
    })

  // Used both when a start was refused and when the user asks to revisit a
  // decision. Asking the agent means the dialog shows the real owner and a
  // genuinely free port, rather than a guess made in the browser.
  const resolve = async (unit: Unit) => {
    const customised =
      !!unit.substitute || unit.ports.some((p) => p.declared && p.declared !== p.configured)
    if (customised || unit.running) {
      try {
        openDialog(unit, await api.options(project.id, unit.id))
      } catch (err) {
        onNotice(err instanceof ApiError ? err.message : String(err), true)
      }
      return
    }
    try {
      onUpdate(await api.unitAction(project.id, unit.id, 'start'))
    } catch (err) {
      if (err instanceof ApiError && err.conflict) {
        openDialog(unit, err.conflict)
      } else if (err instanceof ApiError) {
        onNotice(`${project.name}: ${err.message}`, true)
      }
    }
  }

  const copyPath = async () => {
    try {
      // Available on localhost, which is a secure context; the fallback covers
      // the agent being reached by some other hostname.
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(project.path)
      } else {
        legacyCopy(project.path)
      }
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      onNotice(`Could not copy. The path is ${project.path}`, true)
    }
  }

  const open = async (url?: string) => {
    try {
      const res = await api.open(project.id, url)
      onNotice(`Opened ${res.opened}`)
    } catch (err) {
      onNotice(err instanceof ApiError ? err.message : String(err), true)
    }
  }

  // Hand a unit back to this project: drop any substitute, then drop any port
  // override. Scoped to one row, so it cannot undo a choice made for a sibling.
  const resetUnit = (unit: Unit) =>
    call(async () => {
      if (unit.substitute) await api.substitute(project.id, unit.id, '')
      return api.unitAction(project.id, unit.id, unit.running ? 'kill' : 'start', {
        clearOverrides: true,
      })
    }, `reset:${unit.id}`)

  const clearSubstitute = (unit: Unit) =>
    call(() => api.substitute(project.id, unit.id, ''), `unlink:${unit.id}`)

  const substitute = (unit: Unit, target: string) =>
    call(() => api.substitute(project.id, unit.id, target), `sub:${unit.id}`)

  return (
    <article className={`card${st.ports.some((p) => p.state === 'conflict') ? ' is-conflict' : ''}`}>
      <div className="card-top">
        <span className={`dot ${st.status}`} aria-hidden="true" />
        <span className="card-name">{project.name}</span>
        <span className="card-kind">{KIND_LABEL[project.kind] ?? project.kind}</span>
        {project.branch && (
          <span className="card-branch" title={`git branch: ${project.branch}`}>
            <svg width="10" height="10" viewBox="0 0 16 16" aria-hidden="true" fill="currentColor">
              <path d="M5 3.25a.75.75 0 1 1-1.5 0 .75.75 0 0 1 1.5 0Zm0 2.122a2.25 2.25 0 1 0-1.5 0v.878A2.25 2.25 0 0 0 5.75 8.5h1.5v2.128a2.251 2.251 0 1 0 1.5 0V8.5h1.5a2.25 2.25 0 0 0 2.25-2.25v-.878a2.25 2.25 0 1 0-1.5 0v.878a.75.75 0 0 1-.75.75h-4.5A.75.75 0 0 1 5 6.25v-.878ZM11 2.5a.75.75 0 1 1 0 1.5.75.75 0 0 1 0-1.5Zm-3 10a.75.75 0 1 1 0 1.5.75.75 0 0 1 0-1.5Z" />
            </svg>
            {project.branch}
          </span>
        )}
        <span className="sr-only">status: {st.status}</span>
        <span className="card-categories">
          {project.categories.map((c) => (
            <span key={c} className="cat-chip">
              {c}
            </span>
          ))}
          <span className="cat-wrap">
            <button
              className="cat-add"
              onClick={() => setEditingCategories((v) => !v)}
              title="Add or edit categories"
              aria-label="Edit categories"
            >
              {project.categories.length ? '⋯' : '+ category'}
            </button>
            {editingCategories && (
              <CategoryEditor
                current={project.categories}
                known={knownCategories}
                busy={busy}
                onClose={() => setEditingCategories(false)}
                onSave={(cats) => {
                  setEditingCategories(false)
                  void call(() => api.setCategories(project.id, cats), 'categories')
                }}
              />
            )}
          </span>
        </span>
        <span className="card-summary">{summary(project)}</span>
      </div>

      <div className="card-detail">
        <span>{statusLine(project)}</span>
        <span className="card-path" title={project.path}>
          {shortenPath(project.path)}
        </span>
        <button
          className="copy-path"
          onClick={copyPath}
          title={`Copy ${project.path}`}
          aria-label="Copy the full project path"
        >
          {copied ? '✓ copied' : 'copy path'}
        </button>
      </div>

      {st.error && (
        <div className="notice bad">
          <span className="grow">{st.error}</span>
        </div>
      )}

      {units.length > 0 && (
        <ul className="units">
          {units.map((u) => (
            <UnitRow
              key={u.id + (u.containerName ?? '')}
              unit={u}
              canOpenBrowser={canOpenBrowser}
              runtimeBlocked={runtimeBlocked}
              busy={busy}
              onStart={() => unitAction(u, 'start')}
              onResolve={() => resolve(u)}
              onClearSubstitute={() => clearSubstitute(u)}
              onKill={() => unitAction(u, 'kill')}
              onOpen={() => open(u.url)}
            />
          ))}
        </ul>
      )}

      <div className="card-actions">
        {multiUnit && (
          <>
            <button
              className={anyRunning ? undefined : 'primary'}
              onClick={() => projectAction(anyRunning ? 'restart' : 'start')}
              disabled={busy || !!runtimeBlocked}
              title={runtimeBlocked ?? undefined}
            >
              {anyRunning ? 'Restart all' : 'Start all'}
            </button>
            {anyRunning && (
              <button
                onClick={() => projectAction('stop')}
                disabled={busy || !!runtimeBlocked}
                title={runtimeBlocked ?? undefined}
              >
                Stop all
              </button>
            )}
          </>
        )}
        <button className="ghost" onClick={() => setShowLogs((v) => !v)}>
          {showLogs ? 'Hide logs' : 'Logs'}
        </button>
      </div>

      {showLogs && <LogPanel project={project} onClose={() => setShowLogs(false)} />}

      {dialog && (
        <ResolveDialog
          conflict={dialog.conflict}
          onCancel={() => setDialog(null)}
          onStart={(opts) => void dialog.run(opts)}
          onSubstitute={(target) => void dialog.substitute(target)}
          onReset={() => void dialog.reset()}
          current={dialog.current}
        />
      )}
    </article>
  )
}

function summary(p: Project): string {
  const units = p.state.units
  if (units.length === 0) return ''
  const running = units.filter((u) => u.running).length
  const noun = p.runtime === 'compose' ? 'container' : 'app'
  const plural = units.length === 1 ? noun : `${noun}s`
  return `${running}/${units.length} ${plural} running`
}

function statusLine(p: Project): string {
  const st = p.state
  // Nothing here for a conflict: the offending row is highlighted, its chip is
  // red, and the popover on that chip has the detail. A count in the header
  // only repeats what the rows already show.
  if (st.status === 'conflict') return ''
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

// Clipboard fallback for a non-secure context, where navigator.clipboard is
// undefined and the only route is a throwaway selection.
function legacyCopy(text: string) {
  const el = document.createElement('textarea')
  el.value = text
  el.setAttribute('readonly', '')
  el.style.position = 'fixed'
  el.style.opacity = '0'
  document.body.appendChild(el)
  el.select()
  document.execCommand('copy')
  document.body.removeChild(el)
}

function shortenPath(path: string): string {
  const parts = path.split('/')
  return parts.length > 4 ? `…/${parts.slice(-3).join('/')}` : path
}

// Kept exported-free but referenced by the card's conflict class computation.
export type { Port }
