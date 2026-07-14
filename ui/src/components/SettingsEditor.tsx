import { useEffect, useId, useState } from 'preact/hooks'
import { SETTINGS_CATALOG, type SettingsCatalogEntry } from '../settings-catalog.gen'
import { IconRefresh } from '../icons'

// SettingsEditor is the catalog-driven form shared by the Config view's
// Defaults tab (the server's global override layer) and the Keys view's
// per-key settings editor. Both write a *partial* ClientSettings — a field
// is either explicitly set (serialized) or absent (inherits from the next
// layer down) — so this component works entirely in terms of that
// partiality rather than a fully-resolved settings object: `values` holds
// only the explicit fields, `isSet` distinguishes "explicitly false/0" from
// "not set", and `placeholders` supplies what to display/pre-fill for an
// unset field (the built-in catalog default, or — for the Defaults tab —
// the currently resolved global value).
export interface SettingsEditorProps {
  values: Record<string, unknown>
  placeholders?: Record<string, unknown>
  onSet: (key: string, value: unknown) => void
  onReset: (key: string) => void
  readOnly?: boolean
}

export function SettingsEditor({ values, placeholders, onSet, onReset, readOnly }: SettingsEditorProps) {
  return (
    <div class="settings-editor">
      {SETTINGS_CATALOG.map((field) => (
        <SettingsFieldRow
          key={field.key}
          field={field}
          isSet={Object.prototype.hasOwnProperty.call(values, field.key)}
          value={values[field.key]}
          placeholder={placeholders?.[field.key] ?? field.default}
          onSet={(v) => onSet(field.key, v)}
          onReset={() => onReset(field.key)}
          readOnly={readOnly}
        />
      ))}
    </div>
  )
}

function SettingsFieldRow({
  field,
  isSet,
  value,
  placeholder,
  onSet,
  onReset,
  readOnly,
}: {
  field: SettingsCatalogEntry
  isSet: boolean
  value: unknown
  placeholder: unknown
  onSet: (v: unknown) => void
  onReset: () => void
  readOnly?: boolean
}) {
  // Accessible naming: the visible field-key text IS the control's label.
  // The row layout (label block left, control right) makes the app's usual
  // wrap-in-<label class="field"> pattern (Settings.tsx) awkward, so wire
  // them by id instead. Ids are useId-derived because several editors can be
  // mounted at once (multiple expanded key rows in Keys.tsx), so a bare
  // field.key would collide. Every field kind except array+enum renders one
  // labelable control (for/id); array+enum renders a checkbox GROUP — each
  // box is already named by its own wrapping option label, and the row label
  // names the group as a whole via role="group" + aria-labelledby.
  const uid = useId()
  const isGroup = field.type === 'array' && !!field.enum
  const controlId = `setf-${uid}`
  const labelId = `setf-${uid}-label`
  return (
    <div class="setf-row">
      <div class="setf-label">
        <label class="val mono" id={labelId} for={isGroup ? undefined : controlId} style={{ display: 'block' }}>
          {field.key}
        </label>
        <div class="hint">{field.description}</div>
      </div>
      <div class="setf-control">
        <FieldControl
          field={field}
          value={isSet ? value : placeholder}
          placeholder={placeholder}
          onChange={onSet}
          readOnly={readOnly}
          controlId={controlId}
          labelId={labelId}
        />
        {!readOnly &&
          (isSet ? (
            <button type="button" class="icon-btn" title="Reset to built-in" aria-label={`Reset ${field.key} to built-in`} onClick={onReset}>
              <IconRefresh />
            </button>
          ) : (
            <span class="chip" title="Not explicitly set — inherits from the next layer down">
              inherited
            </span>
          ))}
      </div>
    </div>
  )
}

function FieldControl({
  field,
  value,
  placeholder,
  onChange,
  readOnly,
  controlId,
  labelId,
}: {
  field: SettingsCatalogEntry
  value: unknown
  placeholder: unknown
  onChange: (v: unknown) => void
  readOnly?: boolean
  controlId: string
  labelId: string
}) {
  if (field.type === 'boolean') {
    return (
      <input
        id={controlId}
        type="checkbox"
        checked={!!value}
        disabled={readOnly}
        onChange={(e) => onChange((e.target as HTMLInputElement).checked)}
      />
    )
  }

  if (field.type === 'integer' || field.type === 'number') {
    return (
      <input
        id={controlId}
        class="input mono"
        type="number"
        min={field.min}
        step={field.type === 'integer' ? 1 : 0.05}
        value={(value as number) ?? 0}
        disabled={readOnly}
        style={{ width: '110px' }}
        onInput={(e) => {
          const raw = (e.target as HTMLInputElement).value
          // Ignore an emptied field rather than coercing "" to 0 (Number('')
          // === 0): clearing the box should not silently write a value that
          // can also fall below the field's declared min.
          if (raw.trim() === '') return
          const n = Number(raw)
          if (!Number.isNaN(n)) onChange(n)
        }}
      />
    )
  }

  if (field.type === 'string' && field.enum) {
    return (
      <select
        id={controlId}
        class="select"
        style={{ width: 'auto' }}
        value={value as string}
        disabled={readOnly}
        onChange={(e) => onChange((e.target as HTMLSelectElement).value)}
      >
        {field.enum.map((opt) => (
          <option key={opt} value={opt}>
            {opt}
          </option>
        ))}
      </select>
    )
  }

  if (field.type === 'string') {
    return (
      <input
        id={controlId}
        class="input"
        value={(value as string) ?? ''}
        placeholder={placeholder ? String(placeholder) : '(empty)'}
        disabled={readOnly}
        onInput={(e) => onChange((e.target as HTMLInputElement).value)}
      />
    )
  }

  if (field.type === 'array' && field.enum) {
    const arr = Array.isArray(value) ? (value as string[]) : []
    return (
      <div role="group" aria-labelledby={labelId} style={{ display: 'flex', gap: '10px', flexWrap: 'wrap' }}>
        {field.enum.map((opt) => (
          <label key={opt} class="hint" style={{ display: 'flex', gap: '4px', alignItems: 'center', cursor: readOnly ? 'default' : 'pointer' }}>
            <input
              type="checkbox"
              checked={arr.includes(opt)}
              disabled={readOnly}
              onChange={() => onChange(arr.includes(opt) ? arr.filter((x) => x !== opt) : [...arr, opt])}
            />
            {opt}
          </label>
        ))}
      </div>
    )
  }

  // Plain string array (inject_pretool_tools): a comma-separated free-text
  // input, matching MetaFilter's tag-parsing convention elsewhere in the UI.
  return (
    <StringArrayInput
      controlId={controlId}
      value={value}
      placeholder={placeholder}
      readOnly={readOnly}
      onChange={onChange}
    />
  )
}

// parseTagList splits a comma-separated free-text value into trimmed,
// non-empty tags — the wire form for a plain string-array setting.
function parseTagList(raw: string): string[] {
  return raw
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}

// StringArrayInput is the comma-separated free-text control for a plain string
// array (inject_pretool_tools). It keeps the raw text in local state instead of
// rendering `value.join(', ')` directly: a controlled input driven by the
// parsed array collapses a separator the instant it's typed (typing "Read,"
// re-renders as "Read"), making multi-value edits impossible. The parsed array
// is still committed to the parent on every keystroke; local text only governs
// what the box displays. An external change to `value` (e.g. Reset) re-seeds
// the text, but only when it diverges from what the current text parses to, so
// a keystroke's own onChange round-trip doesn't clobber the in-progress text.
function StringArrayInput({
  controlId,
  value,
  placeholder,
  readOnly,
  onChange,
}: {
  controlId: string
  value: unknown
  placeholder: unknown
  readOnly?: boolean
  onChange: (v: unknown) => void
}) {
  const arr = Array.isArray(value) ? (value as string[]) : []
  const [text, setText] = useState(arr.join(', '))
  useEffect(() => {
    if (parseTagList(text).join(' ') !== arr.join(' ')) {
      setText(arr.join(', '))
    }
    // Re-seed only on an external value change, not on every text edit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value])
  return (
    <input
      id={controlId}
      class="input mono"
      value={text}
      placeholder={Array.isArray(placeholder) ? (placeholder as string[]).join(', ') : ''}
      disabled={readOnly}
      onInput={(e) => {
        const raw = (e.target as HTMLInputElement).value
        setText(raw)
        onChange(parseTagList(raw))
      }}
    />
  )
}

// formatSettingValue renders a resolved (non-partial) settings value for the
// read-only visibility table (Config's Settings tab) — same type switch as
// FieldControl but to a display string rather than an editable control.
export function formatSettingValue(entry: SettingsCatalogEntry, value: unknown): string {
  if (entry.type === 'boolean') return value ? 'true' : 'false'
  if (entry.type === 'array') {
    const arr = Array.isArray(value) ? value : []
    return arr.length ? arr.join(', ') : '(empty)'
  }
  if (value === '' || value === undefined || value === null) return '(empty)'
  return String(value)
}
