// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useEffect, useId, useState } from 'react'

interface NodeProperty {
  name: string
  displayName: string
  type: string
  default?: unknown
  required?: boolean
  description?: string
  placeholder?: string
  options?: { name: string; value: unknown; description?: string }[]
  subProperties?: NodeProperty[]
}

interface Props {
  property: NodeProperty
  value: unknown
  onChange: (value: unknown) => void
}

export default function DynamicField({ property, value, onChange }: Props) {
  const label = property.displayName || property.name
  const fieldId = useId()

  switch (property.type) {
    case 'string':
      return (
        <FieldWrapper id={fieldId} label={label} description={property.description} required={property.required}>
          <input
            id={fieldId}
            type="text"
            value={(value as string) ?? property.default ?? ''}
            onChange={e => onChange(e.target.value)}
            placeholder={property.placeholder}
            className="field-input"
          />
        </FieldWrapper>
      )

    case 'number': {
      const numericFallback = (() => {
        if (typeof value === 'number') return String(value)
        if (typeof property.default === 'number') return String(property.default)
        return ''
      })()
      return (
        <FieldWrapper id={fieldId} label={label} description={property.description} required={property.required}>
          <input
            id={fieldId}
            type="number"
            value={numericFallback}
            onChange={e => {
              const v = e.target.value
              if (v === '') {
                onChange(null)
                return
              }
              const n = Number(v)
              if (!isNaN(n)) onChange(n)
            }}
            placeholder={property.placeholder}
            className="field-input"
          />
        </FieldWrapper>
      )
    }

    case 'boolean':
      // Booleans render their own inline <label> wrapping the checkbox so the
      // click target includes the visible text. Skip FieldWrapper's outer
      // label to avoid two <label htmlFor=> nodes pointing at the same input,
      // which screen readers announce twice.
      return (
        <div>
          <label htmlFor={fieldId} className="flex items-center gap-2 cursor-pointer">
            <input
              id={fieldId}
              type="checkbox"
              checked={Boolean(value ?? property.default ?? false)}
              onChange={e => onChange(e.target.checked)}
              className="rounded border-zinc-600 bg-zinc-800 text-indigo-500 focus:ring-indigo-500 focus:ring-offset-0"
            />
            <span className="text-[10px] text-zinc-400">{label}</span>
          </label>
          {property.description && <div className="text-[9px] text-zinc-600 mt-0.5">{property.description}</div>}
        </div>
      )

    case 'options': {
      const opts = property.options || []
      // Map stringified value back to its typed counterpart so we don't lose
      // booleans/numbers when the user picks an option.
      const currentValue = value ?? property.default
      const selectedKey = currentValue !== undefined && currentValue !== null
        ? String(currentValue)
        : ''
      return (
        <FieldWrapper id={fieldId} label={label} description={property.description} required={property.required}>
          <select
            id={fieldId}
            value={selectedKey}
            onChange={e => {
              const picked = e.target.value
              if (picked === '') {
                onChange(null)
                return
              }
              // Preserve the original typed value rather than coercing to string.
              const match = opts.find(o => String(o.value) === picked)
              onChange(match ? match.value : picked)
            }}
            className="field-input"
          >
            <option value="">Select...</option>
            {opts.map(opt => (
              <option key={String(opt.value)} value={String(opt.value)}>
                {opt.name}
              </option>
            ))}
          </select>
        </FieldWrapper>
      )
    }

    case 'multiOptions': {
      const selected = Array.isArray(value) ? (value as unknown[]) : []
      const opts = property.options || []
      return (
        <FieldWrapper id={fieldId} label={label} description={property.description}>
          <div className="max-h-28 overflow-y-auto bg-zinc-800 rounded border border-zinc-700 p-1.5 space-y-0.5">
            {opts.map(opt => {
              // Object/array option values would collide on String(opt.value)
              // (both render as "[object Object]") and silently merge into one
              // checkbox. Reject them at runtime so the schema author sees the
              // problem during dev rather than discovering it via duplicate
              // selections in production.
              const t = typeof opt.value
              if (t !== 'string' && t !== 'number' && t !== 'boolean' && opt.value !== null) {
                console.warn('multiOptions: non-primitive option value rejected', opt)
                return null
              }
              const isChecked = selected.some(s => s === opt.value || String(s) === String(opt.value))
              return (
                <label key={String(opt.value)} className="flex items-center gap-1.5 px-1 py-0.5 hover:bg-zinc-700/50 rounded cursor-pointer">
                  <input
                    type="checkbox"
                    checked={isChecked}
                    onChange={e => {
                      if (e.target.checked) {
                        // Avoid duplicate insertion on toggle re-checks.
                        if (selected.some(s => s === opt.value || String(s) === String(opt.value))) return
                        onChange([...selected, opt.value])
                      } else {
                        onChange(selected.filter(s => s !== opt.value && String(s) !== String(opt.value)))
                      }
                    }}
                    className="rounded border-zinc-600 bg-zinc-800 text-indigo-500 focus:ring-indigo-500 focus:ring-offset-0"
                  />
                  <span className="text-[10px] text-zinc-300">{opt.name}</span>
                </label>
              )
            })}
          </div>
        </FieldWrapper>
      )
    }

    case 'json':
      return (
        <JsonField
          id={fieldId}
          label={label}
          description={property.description}
          required={property.required}
          value={value}
          defaultValue={property.default}
          onChange={onChange}
        />
      )

    case 'collection':
    case 'fixedCollection':
      return (
        <FieldWrapper id={fieldId} label={label} description={property.description}>
          <div className="bg-zinc-800/50 rounded border border-zinc-700 p-2 space-y-2">
            {(property.subProperties || []).map(sub => (
              <DynamicField
                key={sub.name}
                property={sub}
                value={(value as Record<string, unknown>)?.[sub.name]}
                onChange={v => onChange({ ...(value as Record<string, unknown> || {}), [sub.name]: v })}
              />
            ))}
          </div>
        </FieldWrapper>
      )

    default:
      // Fallback to string input
      return (
        <FieldWrapper id={fieldId} label={label} description={property.description} required={property.required}>
          <input
            id={fieldId}
            type="text"
            value={String(value ?? property.default ?? '')}
            onChange={e => onChange(e.target.value)}
            placeholder={property.placeholder}
            className="field-input"
          />
        </FieldWrapper>
      )
  }
}

function FieldWrapper({ id, label, description, required, children }: {
  id?: string; label: string; description?: string; required?: boolean; children: React.ReactNode
}) {
  return (
    <div>
      <label htmlFor={id} className="block text-[10px] text-zinc-500 mb-0.5">
        {label}{required && <span className="text-red-400 ml-0.5">*</span>}
      </label>
      {children}
      {description && <div className="text-[9px] text-zinc-600 mt-0.5">{description}</div>}
    </div>
  )
}

interface JsonFieldProps {
  id: string
  label: string
  description?: string
  required?: boolean
  value: unknown
  defaultValue: unknown
  onChange: (value: unknown) => void
}

// Reject inputs larger than this — pasting megabytes of JSON freezes the parser
// and the UI. 1MB is generous for a config field.
const MAX_JSON_LENGTH = 1024 * 1024

function JsonField({ id, label, description, required, value, defaultValue, onChange }: JsonFieldProps) {
  const initialText = (() => {
    if (typeof value === 'string') return value
    try {
      return JSON.stringify(value ?? defaultValue ?? {}, null, 2)
    } catch {
      return ''
    }
  })()

  const [text, setText] = useState<string>(initialText)
  const [error, setError] = useState<string | null>(null)

  // Sync external value changes back into local text when not actively editing an invalid value.
  useEffect(() => {
    if (error) return
    if (typeof value === 'string') {
      if (value !== text) setText(value)
      return
    }
    let next: string
    try {
      next = JSON.stringify(value ?? defaultValue ?? {}, null, 2)
    } catch {
      return
    }
    if (next !== text) setText(next)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value, defaultValue])

  // Debounced parse: avoids running JSON.parse on every keystroke for large
  // inputs. Empty input clears the value immediately so consumers don't see
  // stale state when the field is wiped.
  useEffect(() => {
    if (text.length > MAX_JSON_LENGTH) {
      setError(`Input too large (${text.length} bytes, max ${MAX_JSON_LENGTH})`)
      return
    }
    if (text.trim() === '') {
      setError(null)
      onChange(null)
      return
    }
    const t = setTimeout(() => {
      try {
        const parsed = JSON.parse(text)
        setError(null)
        onChange(parsed)
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Invalid JSON')
      }
    }, 200)
    return () => clearTimeout(t)
    // onChange is intentionally omitted — parents typically pass an inline
    // closure, which would otherwise retrigger the debounce on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [text])

  const invalid = error !== null

  return (
    <FieldWrapper id={id} label={label} description={description} required={required}>
      <textarea
        id={id}
        value={text}
        onChange={e => setText(e.target.value)}
        onPaste={e => {
          const pasted = e.clipboardData.getData('text')
          if (pasted.length > MAX_JSON_LENGTH) {
            e.preventDefault()
            const msg = `Pasted content too large (${pasted.length} bytes, max ${MAX_JSON_LENGTH}). Reduce size and try again.`
            setError(msg)
            // Auto-clear the paste-rejection error after 5s so the user can
            // resume editing without typing extra characters first. Without
            // this, the error sits visible until the next keystroke triggers
            // the debounce effect and overwrites it.
            setTimeout(() => {
              setError(prev => prev === msg ? null : prev)
            }, 5000)
          }
        }}
        rows={4}
        className={`field-input font-mono text-[10px] resize-y ${invalid ? '!border-red-500 focus:!border-red-500' : ''}`}
        placeholder="{}"
        aria-invalid={invalid}
      />
      {invalid && (
        <div className="text-[9px] text-red-400 mt-0.5">Invalid JSON: {error}</div>
      )}
    </FieldWrapper>
  )
}

export type { NodeProperty }