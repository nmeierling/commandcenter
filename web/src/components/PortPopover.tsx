import { useEffect, useRef } from 'react'
import type { UnitPort } from '../types'

interface Props {
  port: UnitPort
  /** Offered when this port is actually blocked. */
  onResolve?: () => void
  onClose: () => void
}

/**
 * Explains one port, anchored to its chip.
 *
 * "port 4200 is occupied by node (pid 86144)" used to sit in the card's summary
 * line, where it pushed the actually-scannable information out of view and said
 * nothing about *which* row it referred to. Attached to the chip, the sentence
 * has an obvious subject.
 */
export function PortPopover({ port, onResolve, onClose }: Props) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    const onDown = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) onClose()
    }
    window.addEventListener('keydown', onKey)
    // Deferred: the click that opened this must not immediately close it.
    const t = window.setTimeout(() => window.addEventListener('mousedown', onDown), 0)
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('mousedown', onDown)
      window.clearTimeout(t)
    }
  }, [onClose])

  const owner = port.owner
  const shown = port.configured || port.actual

  return (
    <div className={`port-popover ${port.state}`} ref={ref} role="dialog" aria-label={`Port ${shown}`}>
      <div className="pp-head">
        <strong>Port {shown}</strong>
        <span className={`pp-state ${port.state}`}>{stateLabel(port)}</span>
      </div>

      <dl className="pp-facts">
        {!!port.configured && (
          <>
            <dt>configured</dt>
            <dd>{port.configured}</dd>
          </>
        )}
        {!!port.declared && port.declared !== port.configured && (
          <>
            <dt>declared</dt>
            <dd>
              {port.declared} <span className="pp-hint">in the project</span>
            </dd>
          </>
        )}
        <>
          <dt>actual</dt>
          <dd>{port.actual ? port.actual : 'not listening'}</dd>
        </>
        {!!port.container && (
          <>
            <dt>container</dt>
            <dd>{port.container}</dd>
          </>
        )}
      </dl>

      {port.state === 'conflict' && (
        <p className="pp-message">
          Port <strong>{shown}</strong> is occupied by <strong>{ownerLabel(port)}</strong>.
        </p>
      )}
      {port.state === 'unknown-owner' && (
        <p className="pp-message">
          Something is listening on <strong>{shown}</strong>, but it could not be attributed —
          usually another user's process.
        </p>
      )}
      {port.state === 'substituted' && (
        <p className="pp-message">
          Served by the substitute you chose, so this project does not start its own.
        </p>
      )}

      {owner?.command && <code className="pp-command">{owner.command}</code>}

      {port.state === 'conflict' && onResolve && (
        <div className="pp-actions">
          <button className="primary" onClick={onResolve}>
            Resolve…
          </button>
        </div>
      )}
    </div>
  )
}

function stateLabel(port: UnitPort): string {
  switch (port.state) {
    case 'owned':
      return 'served by this project'
    case 'conflict':
      return 'in use by something else'
    case 'unknown-owner':
      return 'in use, owner unknown'
    case 'substituted':
      return 'substituted'
    default:
      return 'free'
  }
}

function ownerLabel(port: UnitPort): string {
  const o = port.owner
  if (!o) return 'an unidentified process'
  if (o.composeProject) return `container ${o.container} (compose project “${o.composeProject}”)`
  if (o.container) return `container ${o.container}`
  if (o.process) return `${o.process} (pid ${o.pid})`
  return `pid ${o.pid}`
}
