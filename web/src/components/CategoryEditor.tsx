import { useEffect, useRef, useState } from 'react'

interface Props {
  current: string[]
  /** Every category already in use, offered as completions. */
  known: string[]
  busy: boolean
  onSave: (categories: string[]) => void
  onClose: () => void
}

/**
 * Edits a project's categories as a comma-separated list.
 *
 * A free-text field rather than a picker: on a fresh dashboard there is nothing
 * to pick from, and typing "backend, team-a" is faster than opening a manager
 * to define them first. Existing categories are offered as completions, which
 * covers the common case of reusing one without making it a precondition.
 */
export function CategoryEditor({ current, known, busy, onSave, onClose }: Props) {
  const [value, setValue] = useState(current.join(', '))
  const ref = useRef<HTMLInputElement>(null)
  const box = useRef<HTMLDivElement>(null)

  useEffect(() => {
    ref.current?.focus()
    ref.current?.select()
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    const onDown = (e: MouseEvent) => {
      if (!box.current?.contains(e.target as Node)) onClose()
    }
    window.addEventListener('keydown', onKey)
    const t = window.setTimeout(() => window.addEventListener('mousedown', onDown), 0)
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('mousedown', onDown)
      window.clearTimeout(t)
    }
  }, [onClose])

  const save = () => onSave(value.split(',').map((c) => c.trim()).filter(Boolean))

  return (
    <div className="cat-editor" ref={box} role="dialog" aria-label="Categories">
      <label htmlFor="cat-input">Categories, separated by commas</label>
      <input
        id="cat-input"
        ref={ref}
        type="text"
        list="devcc-known-categories"
        value={value}
        placeholder="backend, team-a"
        spellCheck={false}
        onChange={(e) => setValue(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') save()
        }}
      />
      <datalist id="devcc-known-categories">
        {known.map((c) => (
          <option key={c} value={c} />
        ))}
      </datalist>
      <p className="hint">
        Each becomes a tab. Saved to your config file. Leave empty to remove the project from
        every tab but <strong>All</strong>.
      </p>
      <div className="cat-editor-actions">
        <button onClick={onClose}>Cancel</button>
        <button className="primary" onClick={save} disabled={busy}>
          Save
        </button>
      </div>
    </div>
  )
}
