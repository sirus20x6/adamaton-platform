// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useId, useRef } from 'react'

interface Props {
  value: string
  onChange: (value: string) => void
  inputFields?: string[]
}

export default function PromptEditor({ value, onChange, inputFields = [] }: Props) {
  const id = useId()
  const textareaRef = useRef<HTMLTextAreaElement | null>(null)

  // Insert a token at the current cursor position (or replacing the selection).
  // Falls back to appending if the textarea hasn't been focused yet — typical
  // first-click flow before the user has interacted with the editor.
  const insertAtCursor = (token: string) => {
    const ta = textareaRef.current
    if (!ta) {
      onChange(value + token)
      return
    }
    const start = ta.selectionStart ?? value.length
    const end = ta.selectionEnd ?? value.length
    const next = value.slice(0, start) + token + value.slice(end)
    onChange(next)
    // Restore cursor *after* React applies the new value. Setting it inline
    // would race with the controlled re-render and the caret would jump.
    requestAnimationFrame(() => {
      const node = textareaRef.current
      if (!node) return
      const pos = start + token.length
      node.focus()
      node.setSelectionRange(pos, pos)
    })
  }

  return (
    <div>
      <label htmlFor={id} className="block text-[10px] text-zinc-500 mb-1">Agent Prompt</label>
      <textarea
        ref={textareaRef}
        id={id}
        value={value}
        onChange={e => onChange(e.target.value)}
        rows={8}
        placeholder="Enter the prompt that will be sent to the AI agent. Use {{.fieldName}} to reference input fields."
        className="w-full bg-zinc-800 border border-zinc-700 rounded px-2.5 py-2 text-xs text-zinc-200 placeholder:text-zinc-600 focus:outline-none focus:border-indigo-500 transition-colors font-mono leading-relaxed resize-y"
      />
      {inputFields.length > 0 && (
        <div className="mt-1.5">
          <div className="text-[9px] text-zinc-500 mb-1">Available variables:</div>
          <div className="flex flex-wrap gap-1">
            {inputFields.map(field => (
              <button
                key={field}
                type="button"
                onClick={() => insertAtCursor(`{{.${field}}}`)}
                className="px-1.5 py-0.5 bg-zinc-800 border border-zinc-700 rounded text-[9px] text-indigo-400 hover:bg-zinc-700 hover:text-indigo-300 transition-colors font-mono"
              >
                {'{{.'}{field}{'}}'}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}