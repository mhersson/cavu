export default function Choice<T extends string>({ id, label, options, value, onChange }: {
  id: string
  label: string
  options: [T, string][]
  value: T
  onChange: (v: T) => void
}) {
  return (
    <div className="frow">
      <label id={id}>{label}</label>
      <div className="seg" role="radiogroup" aria-labelledby={id}>
        {options.map(([v, text]) => (
          <button key={v} role="radio" aria-checked={v === value} className={v === value ? 'on' : ''} onClick={() => onChange(v)}>
            {text}
          </button>
        ))}
      </div>
    </div>
  )
}
