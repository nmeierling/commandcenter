import { useState } from 'react'

import type { Unit, UnitPort } from '../types'
import { PortPopover } from './PortPopover'

interface Props {
  unit: Unit
  /** True when the agent can launch the machine's default browser. */
  canOpenBrowser: boolean
  /** Set when the whole runtime is unavailable (docker daemon down). */
  runtimeBlocked: string | null
  busy: boolean
  onStart: () => void
  onResolve: () => void
  onClearSubstitute: () => void
  onKill: () => void
  onOpen: () => void
}

/**
 * One container or supervised app, listed whatever its state.
 *
 * The action is chosen by what the row can actually do right now: Kill while it
 * is running, Start when its port is free, and Start other… when something else
 * holds the port. Offering all three at once would mean two of them are wrong.
 */
export function UnitRow({
  unit,
  canOpenBrowser,
  runtimeBlocked,
  busy,
  onStart,
  onResolve,
  onClearSubstitute,
  onKill,
  onOpen,
}: Props) {
  const conflicted = unit.ports.some((p) => p.state === 'conflict')
  const blocked = unit.blocked || runtimeBlocked
  const moved = unit.ports.filter((p) => p.declared && p.declared !== p.configured)
  const sub = unit.substitute
  // Which port chip has its popover open, by index.
  const [openPort, setOpenPort] = useState<number | null>(null)
  // A row whose behaviour has been customised can revisit that decision.
  const customised = !!sub || moved.length > 0

  return (
    <li className={`unit-row${conflicted ? ' has-conflict' : ''}${sub?.satisfied ? ' is-substituted' : ''}`}>
      <span className={`dot ${dotClass(unit)}`} aria-hidden="true" />
      <span className="unit-name" title={unit.containerName || unit.name}>
        {unit.name}
      </span>
      {sub && (
        <span className={`unit-tag${sub.satisfied ? '' : ' bad'}`}>
          {sub.satisfied ? 'substituted' : 'substitute down'}
        </span>
      )}
      <span className="unit-state" title={unit.image}>
        {stateLabel(unit)}
      </span>

      <span className="unit-ports">
        {unit.ports.length === 0 ? (
          <span className="muted">no ports</span>
        ) : (
          unit.ports.map((p, i) => (
            <PortChip
              key={`${p.configured}-${p.actual}-${i}`}
              port={p}
              open={openPort === i}
              onToggle={() => setOpenPort(openPort === i ? null : i)}
              onResolve={() => {
                setOpenPort(null)
                onResolve()
              }}
            />
          ))
        )}
      </span>

      <span className="unit-actions">
        {customised && (
          <button
            className="ghost tiny"
            onClick={onResolve}
            disabled={busy}
            title="Change how this unit is resolved"
          >
            change…
          </button>
        )}
        {unit.url &&
          (canOpenBrowser ? (
            <button className="row-open" onClick={onOpen} title={`Open ${unit.url}`}>
              Open
            </button>
          ) : (
            <a
              className="row-open as-button"
              href={unit.url}
              target="_blank"
              rel="noopener noreferrer"
              title={`Open ${unit.url} in a new tab`}
            >
              Open
            </a>
          ))}

        {sub?.satisfied ? (
          <button className="ghost" onClick={onClearSubstitute} disabled={busy} title="Stop using the substitute and manage this unit again">
            Unlink
          </button>
        ) : unit.running ? (
          <button className="danger" onClick={onKill} disabled={busy || !!blocked} title={blocked ?? 'Stop this one'}>
            Kill
          </button>
        ) : conflicted ? (
          <button
            onClick={onResolve}
            disabled={busy || !!blocked}
            title={blocked ?? 'Its port is taken — choose how to resolve it'}
          >
            Resolve…
          </button>
        ) : (
          <button
            className="primary"
            onClick={onStart}
            disabled={busy || !!blocked}
            title={blocked ?? 'Start this one'}
          >
            Start
          </button>
        )}
      </span>

      {/* Only the substitute gets a note: the provider's identity appears
          nowhere else. A moved port is fully described by its own chip. */}
      {sub && (
        <span className="unit-note">
          {sub.satisfied ? (
            <>
              provided by{' '}
              <strong>
                {sub.projectName}/{sub.unitId}
              </strong>
              {sub.ports?.length ? ` on :${sub.ports.join(', :')}` : ''}
            </>
          ) : (
            <span className="bad-text">
              substitute {sub.target} unavailable — {sub.detail}
            </span>
          )}
        </span>
      )}
    </li>
  )
}

/**
 * Shows the configured port and, when they differ, the port actually in use.
 * Those two numbers disagreeing is exactly the situation worth surfacing.
 */
function PortChip({
  port,
  open,
  onToggle,
  onResolve,
}: {
  port: UnitPort
  open: boolean
  onToggle: () => void
  onResolve: () => void
}) {
  // Show the port devcc will use. Before anything starts there is no actual
  // port, and falling back to the declared one hid the whole point of a move.
  const moved = !!port.declared && port.declared !== port.configured
  const shown = port.configured || port.actual
  const body = (
    <>
      <span className="uport-host">{shown}</span>
      {port.container ? (
        <>
          <span className="arrow" aria-hidden="true">
            →
          </span>
          <span className="uport-target">{port.container}</span>
        </>
      ) : null}
      {/* Only annotate when the project's own value differs; an untouched row
          needs no label, the muted styling already says it. */}
      {moved && <span className="uport-was">was {port.declared}</span>}
    </>
  )

  return (
    <span className="uport-wrap">
      <button
        type="button"
        className={`uport ${port.state}${moved ? ' moved' : ''}`}
        aria-expanded={open}
        title={chipTitle(port)}
        onClick={onToggle}
      >
        {body}
      </button>
      {open && (
        <PortPopover port={port} onResolve={onResolve} onClose={onToggle} />
      )}
    </span>
  )
}

function chipTitle(port: UnitPort): string {
  const bits: string[] = []
  if (port.configured) bits.push(`configured: ${port.configured}`)
  bits.push(port.actual ? `actual: ${port.actual}` : 'not listening')
  if (port.container) bits.push(`container port: ${port.container}`)
  switch (port.state) {
    case 'conflict':
      bits.push(`held by ${ownerLabel(port)}`)
      break
    case 'owned':
      bits.push('served by this project')
      break
    case 'unknown-owner':
      bits.push('in use, owner unknown')
      break
    case 'free':
      bits.push('free')
      break
    case 'substituted':
      bits.push('served by the substitute you chose')
      break
  }
  return bits.join(' · ')
}

function ownerLabel(port: UnitPort): string {
  const o = port.owner
  if (!o) return 'an unidentified process'
  if (o.composeProject) return `${o.container} (compose project ${o.composeProject})`
  if (o.container) return o.container
  if (o.process) return `${o.process} (pid ${o.pid})`
  return `pid ${o.pid}`
}

function dotClass(unit: Unit): string {
  switch (unit.state) {
    // A satisfied substitution is being served right now — by something else,
    // which the ring says — so it belongs in the same visual family as running
    // rather than looking like an unknown.
    case 'substituted':
      return unit.substitute?.satisfied ? 'running substituted' : 'unknown'
    case 'running':
      return unit.health === 'unhealthy' ? 'unhealthy' : 'running'
    case 'exited':
      return 'stopped'
    case 'created':
      return 'stopped'
    case 'not-created':
      return 'unknown'
    default:
      return 'unknown'
  }
}

function stateLabel(unit: Unit): string {
  if (unit.status) {
    return unit.health ? `${unit.status} · ${unit.health}` : unit.status
  }
  switch (unit.state) {
    case 'running':
      return 'running'
    case 'exited':
      return 'stopped'
    case 'created':
      return 'created, never started'
    case 'not-created':
      return unit.kind === 'container' ? 'not created yet' : 'not started'
    case 'substituted':
      return 'provided elsewhere'
    default:
      return 'unknown'
  }
}
