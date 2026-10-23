import { useEffect, useMemo, useRef, useState } from 'react'
import type { ActionOptions } from '../api'
import type { PortConflict } from '../types'

interface Props {
  conflict: PortConflict
  onCancel: () => void
  /** Start with the chosen port(s), or free the port first. */
  onStart: (opts: ActionOptions) => void
  /** Record that something already running provides this unit. */
  onSubstitute: (target: string) => void
  /** Drop any substitute and port override; run it here as configured. */
  onReset: () => void
  /** The unit as the dashboard currently sees it, when revisiting a decision. */
  current?: { substituted: boolean; moved: boolean }
}

type Mode = 'port' | 'substitute' | 'normal'

/**
 * Asks how to resolve a blocked start.
 *
 * There are two genuinely different answers, and picking the wrong one is
 * costly. Moving to another port gives you a *second* instance — right for a web
 * server, wrong for the database three projects share, because an app talking to
 * an empty second database looks healthy while being useless. Substituting says
 * "the one already running is the one I want", and is remembered.
 */
export function ResolveDialog({
  conflict,
  onCancel,
  onStart,
  onSubstitute,
  onReset,
  current,
}: Props) {
  const candidates = conflict.candidates ?? []
  const canMove = conflict.portControl.canChange
  const canStopAll = conflict.ports.every((p) => p.canStopOwner)

  const customised = !!(current?.substituted || current?.moved)
  // Default to whichever answer is more likely: keep the decision already made
  // when revisiting, otherwise prefer a provider that serves this exact port.
  const [mode, setMode] = useState<Mode>(() => {
    if (current?.substituted) return 'substitute'
    if (current?.moved) return 'port'
    if (candidates.some((c) => c.matches)) return 'substitute'
    return canMove ? 'port' : 'substitute'
  })
  const [target, setTarget] = useState(
    () => candidates.find((c) => c.matches)?.target ?? candidates[0]?.target ?? '',
  )
  const [ports, setPorts] = useState<Record<number, number>>(() =>
    Object.fromEntries(conflict.ports.map((p) => [p.port, p.suggested ?? p.port + 1])),
  )
  const first = useRef<HTMLInputElement>(null)
  const chosen = candidates.find((c) => c.target === target)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCancel()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onCancel])

  const invalidPort = useMemo(() => {
    if (mode !== 'port') return null
    for (const p of conflict.ports) {
      const v = ports[p.port]
      if (!Number.isInteger(v) || v < 1 || v > 65535) return `Port ${v} is not valid.`
      if (v === p.port) return `Port ${v} is the one already in use.`
    }
    return null
  }, [mode, ports, conflict.ports])

  const confirm = () => {
    if (mode === 'normal') {
      onReset()
      return
    }
    if (mode === 'substitute') {
      if (target) onSubstitute(target)
      return
    }
    // Keyed by the declared port: moving an already-moved port has to update
    // that entry rather than add a second one keyed by its current value.
    const mapping: Record<string, number> = {}
    for (const p of conflict.ports) mapping[String(p.declared || p.port)] = ports[p.port]
    onStart({ ports: mapping })
  }

  const disabled =
    mode === 'normal' ? false : mode === 'substitute' ? !target : !canMove || !!invalidPort

  return (
    <div className="modal-backdrop" onMouseDown={onCancel}>
      <div
        className="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="resolve-title"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <h2 id="resolve-title">
          {conflict.blocked ? 'Resolve the port conflict' : 'How should this run?'}
        </h2>
        <p className="modal-sub">
          <strong>{conflict.unitName ?? conflict.projectName}</strong>
          {conflict.unitName ? ` in ${conflict.projectName}` : ''}
          {conflict.blocked ? ' cannot use its configured port.' : ''}
        </p>

        {conflict.blocked ? (
          <ul className="conflict-list">
            {conflict.ports
              .filter((p) => p.canStopOwner || p.owner)
              .map((p) => (
                <li key={p.port}>
                  <code>:{p.port}</code> is held by <strong>{p.ownerLabel}</strong>
                  {p.owner?.command && (
                    <div className="conflict-cmd" title={p.owner.command}>
                      {p.owner.command}
                    </div>
                  )}
                </li>
              ))}
          </ul>
        ) : (
          <p className="modal-sub current-note">
            {current?.substituted
              ? 'It is currently provided by something else.'
              : current?.moved
                ? 'It is currently running on a port you chose.'
                : `Configured for :${conflict.ports.map((p) => p.port).join(', :')}.`}
          </p>
        )}

        <fieldset className="choices">
          <legend className="sr-only">How to resolve</legend>

          <label className={`choice${candidates.length ? '' : ' disabled'}`}>
            <input
              type="radio"
              name="resolve"
              checked={mode === 'substitute'}
              disabled={candidates.length === 0}
              onChange={() => setMode('substitute')}
            />
            <span>
              <strong>Use something already running</strong>
              <span className="choice-note">
                {candidates.length === 0
                  ? 'Nothing else is currently running that publishes a port, so there is nothing to point at.'
                  : 'This project will not start its own copy. Right for a shared database; the choice is written to your config file.'}
              </span>
              {candidates.length > 0 && (
                <>
                  <select
                    className="substitute-select"
                    value={target}
                    disabled={mode !== 'substitute'}
                    onChange={(e) => setTarget(e.target.value)}
                    aria-label="Provider"
                  >
                    {group(candidates, 'project').length > 0 && (
                      <optgroup label="Projects">
                        {group(candidates, 'project').map((c) => (
                          <option key={c.target} value={c.target}>
                            {optionLabel(c)}
                          </option>
                        ))}
                      </optgroup>
                    )}
                    {group(candidates, 'docker').length > 0 && (
                      <optgroup label="Other running containers">
                        {group(candidates, 'docker').map((c) => (
                          <option key={c.target} value={c.target}>
                            {optionLabel(c)}
                          </option>
                        ))}
                      </optgroup>
                    )}
                  </select>
                  {chosen?.source === 'docker' && (
                    <span className="choice-note">
                      <code>{chosen.container}</code> is a container devcc does not manage
                      {chosen.image ? ` (${chosen.image})` : ''}. It is matched by container name,
                      so recreating it under a different name will break the link.
                    </span>
                  )}
                </>
              )}
            </span>
          </label>

          <label className={`choice${canMove ? '' : ' disabled'}`}>
            <input
              type="radio"
              name="resolve"
              checked={mode === 'port'}
              disabled={!canMove}
              onChange={() => setMode('port')}
            />
            <span>
              <strong>Start on a different port</strong>
              <span className="choice-note">
                {canMove
                  ? `${mechanismNote(conflict.portControl.mechanism)} You get a second, separate instance.`
                  : conflict.portControl.reason ||
                    'This project has no port that can be moved safely.'}
              </span>
              {canMove && (
                <span className="port-inputs">
                  {conflict.ports.map((p, i) => (
                    <span key={p.port} className="port-input">
                      <span className="muted">:{p.port}</span>
                      <span aria-hidden="true">→</span>
                      <label className="sr-only" htmlFor={`resolve-port-${p.port}`}>
                        New port for {p.port}
                      </label>
                      <input
                        id={`resolve-port-${p.port}`}
                        ref={i === 0 ? first : undefined}
                        type="number"
                        min={1}
                        max={65535}
                        value={ports[p.port] ?? ''}
                        disabled={mode !== 'port'}
                        onChange={(e) =>
                          setPorts((cur) => ({ ...cur, [p.port]: Number(e.target.value) }))
                        }
                      />
                    </span>
                  ))}
                </span>
              )}
            </span>
          </label>
          {customised && (
            <label className="choice">
              <input
                type="radio"
                name="resolve"
                checked={mode === 'normal'}
                onChange={() => setMode('normal')}
              />
              <span>
                <strong>Run it here, as configured</strong>
                <span className="choice-note">
                  Drops the substitute and any port override, and hands this unit back to the
                  project. Both entries are removed from your config file.
                </span>
              </span>
            </label>
          )}

        </fieldset>

        {invalidPort && (
          <div className="notice bad" style={{ margin: '10px 0 0' }}>
            {invalidPort}
          </div>
        )}

        <div className="modal-actions">
          {canStopAll && conflict.blocked && (
            <button
              className="ghost danger"
              onClick={() => onStart({ stopOwners: true })}
              title="Stop whatever holds the port, then start this here"
            >
              Free the port instead
            </button>
          )}
          <span style={{ flex: 1 }} />
          <button onClick={onCancel}>Cancel</button>
          <button className="primary" onClick={confirm} disabled={disabled}>
            {mode === 'normal'
              ? 'Run it here'
              : mode === 'substitute'
                ? 'Use it'
                : `Start on :${ports[conflict.ports[0].port]}`}
          </button>
        </div>
      </div>
    </div>
  )
}

function group(
  candidates: NonNullable<PortConflict['candidates']>,
  source: 'project' | 'docker',
) {
  return candidates.filter((c) => c.source === source)
}

function optionLabel(c: NonNullable<PortConflict['candidates']>[number]): string {
  const where = c.source === 'docker' ? c.container : `${c.projectName} / ${c.unitName}`
  const ports = c.ports.length ? ` — :${c.ports.join(', :')}` : ''
  return `${where}${ports}${c.matches ? '  (serves this port)' : ''}`
}

function mechanismNote(mechanism: PortConflict['portControl']['mechanism']): string {
  switch (mechanism) {
    case 'compose-var':
      return 'Sets the environment variable your compose file already uses.'
    case 'compose-override':
      return 'Adds an agent-owned compose override file; your project is not edited.'
    case 'command':
      return 'Rewrites the --port flag in the start command.'
    case 'env':
      return 'Sets PORT in the process environment; the row will show the port it actually opened.'
    default:
      return ''
  }
}
