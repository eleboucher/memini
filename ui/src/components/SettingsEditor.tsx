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
  return (
    <div class="setf-row">
      <div class="setf-label">
        <div class="val mono">{field.key}</div>
        <div class="hint">{field.description}</div>
      </div>
      <div class="setf-control">
        <FieldControl field={field} value={isSet ? value : placeholder} placeholder={placeholder} onChange={onSet} readOnly={readOnly} />
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
}: {
  field: SettingsCatalogEntry
  value: unknown
  placeholder: unknown
  onChange: (v: unknown) => void
  readOnly?: boolean
}) {
  if (field.type === 'boolean') {
    return (
      <input
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
        class="input mono"
        type="number"
        min={field.min}
        step={field.type === 'integer' ? 1 : 0.05}
        value={(value as number) ?? 0}
        disabled={readOnly}
        style={{ width: '110px' }}
        onInput={(e) => {
          const n = Number((e.target as HTMLInputElement).value)
          if (!Number.isNaN(n)) onChange(n)
        }}
      />
    )
  }

  if (field.type === 'string' && field.enum) {
    return (
      <select
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
      <div style={{ display: 'flex', gap: '10px', flexWrap: 'wrap' }}>
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
  const arr = Array.isArray(value) ? (value as string[]) : []
  return (
    <input
      class="input mono"
      value={arr.join(', ')}
      placeholder={Array.isArray(placeholder) ? (placeholder as string[]).join(', ') : ''}
      disabled={readOnly}
      onInput={(e) =>
        onChange(
          (e.target as HTMLInputElement).value
            .split(',')
            .map((s) => s.trim())
            .filter(Boolean),
        )
      }
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
