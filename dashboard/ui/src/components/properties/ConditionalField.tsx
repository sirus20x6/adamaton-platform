// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import DynamicField, { type NodeProperty } from './DynamicField'

interface DisplayOptions {
  show?: Record<string, unknown[]>
  hide?: Record<string, unknown[]>
}

interface Props {
  property: NodeProperty & { displayOptions?: DisplayOptions }
  value: unknown
  onChange: (value: unknown) => void
  formValues: Record<string, unknown>
}

export default function ConditionalField({ property, value, onChange, formValues }: Props) {
  if (!isVisible(property.displayOptions, formValues)) {
    return null
  }

  return <DynamicField property={property} value={value} onChange={onChange} />
}

// resolvePath looks up a value by either flat key ("foo"), dotted path
// ("options.batchSize"), or bracketed index ("options.batch[0].size"). Falls
// back to the flat key first to preserve backward compatibility with existing
// form schemas. The split regex handles "." and "[N]" segments uniformly:
// "options.batch[0].size" -> ["options", "batch", "0", "size"].
function resolvePath(values: Record<string, unknown>, key: string): unknown {
  if (Object.prototype.hasOwnProperty.call(values, key)) {
    return values[key]
  }
  if (!key.includes('.') && !key.includes('[')) return undefined
  const parts = key.split(/[.[\]]+/).filter(Boolean)
  let current: unknown = values
  for (const part of parts) {
    if (current === null || current === undefined) return undefined
    if (Array.isArray(current)) {
      const idx = Number(part)
      if (!Number.isInteger(idx)) return undefined
      current = current[idx]
      continue
    }
    if (typeof current !== 'object') return undefined
    current = (current as Record<string, unknown>)[part]
  }
  return current
}

function isVisible(opts: DisplayOptions | undefined, formValues: Record<string, unknown>): boolean {
  if (!opts) return true

  // Show conditions: ALL must match
  if (opts.show) {
    for (const [key, allowedValues] of Object.entries(opts.show)) {
      const current = resolvePath(formValues, key)
      if (!allowedValues.some(v => String(v) === String(current))) {
        return false
      }
    }
  }

  // Hide conditions: ANY match hides the field
  if (opts.hide) {
    for (const [key, hiddenValues] of Object.entries(opts.hide)) {
      const current = resolvePath(formValues, key)
      if (hiddenValues.some(v => String(v) === String(current))) {
        return false
      }
    }
  }

  return true
}