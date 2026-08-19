import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { api, streamUrl } from '../api'
import type { LogLine, Project } from '../types'

interface Props {
  project: Project
  onClose: () => void
}

export function LogPanel({ project, onClose }: Props) {
  const [lines, setLines] = useState<LogLine[]>([])
  const [follow, setFollow] = useState(true)
  const [live, setLive] = useState(false)
  const [note, setNote] = useState<string | null>(null)
  const bodyRef = useRef<HTMLPreElement>(null)
  const seen = useRef(new Set<number>())

  useEffect(() => {
    seen.current = new Set()
    setLines([])
    setNote(null)

    let closed = false
    const source = new EventSource(
      streamUrl(`/api/projects/${project.id}/logs/stream?tail=300`),
    )

    source.addEventListener('open', () => !closed && setLive(true))
    source.addEventListener('line', (ev) => {
      const line = JSON.parse((ev as MessageEvent).data) as LogLine
      // The stream replays a tail after subscribing, so the seam can repeat a
      // few lines. Sequence numbers make that idempotent.
      if (line.seq != null) {
        if (seen.current.has(line.seq)) return
        seen.current.add(line.seq)
      }
      setLines((current) => {
        const next = current.concat(line)
        return next.length > 2000 ? next.slice(next.length - 2000) : next
      })
    })
    source.addEventListener('error', () => {
      setLive(false)
      source.close()
    })

    // Compose logs come from the containers; if the stream cannot attach we
    // still want the snapshot the REST endpoint can provide.
    api
      .logs(project.id, 300)
      .then((res) => {
        if (closed || !res.warning) return
        setNote(res.warning)
      })
      .catch(() => undefined)

    return () => {
      closed = true
      source.close()
    }
  }, [project.id])

  useLayoutEffect(() => {
    if (follow && bodyRef.current) {
      bodyRef.current.scrollTop = bodyRef.current.scrollHeight
    }
  }, [lines, follow])

  const onScroll = () => {
    const el = bodyRef.current
    if (!el) return
    // Turn following off as soon as the user scrolls up, and back on when they
    // return to the bottom.
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
    setFollow(atBottom)
  }

  return (
    <div className="logs">
      <div className="logs-head">
        <span>
          logs · {project.runtime === 'compose' ? 'containers' : 'process'}
        </span>
        <span style={{ color: live ? 'var(--ok)' : 'var(--faint)' }}>
          {live ? '● live' : '○ disconnected'}
        </span>
        {!follow && (
          <button
            className="ghost"
            onClick={() => {
              setFollow(true)
              if (bodyRef.current) {
                bodyRef.current.scrollTop = bodyRef.current.scrollHeight
              }
            }}
          >
            jump to end
          </button>
        )}
        <span style={{ marginLeft: 'auto' }} />
        <button className="ghost" onClick={() => setLines([])}>
          clear
        </button>
        <button className="ghost" onClick={onClose}>
          close
        </button>
      </div>
      {note && (
        <div className="notice warn" style={{ margin: 0, borderRadius: 0 }}>
          {note}
        </div>
      )}
      <pre className="logs-body" ref={bodyRef} onScroll={onScroll} tabIndex={0}>
        {lines.length === 0 ? (
          <span className="empty">
            No output yet
            {project.state.status === 'stopped'
              ? ' — start the project to see logs.'
              : '…'}
          </span>
        ) : (
          lines.map((l, i) => (
            <div key={l.seq ?? i} className={`line-${l.stream}`}>
              {l.text || ' '}
            </div>
          ))
        )}
      </pre>
    </div>
  )
}
