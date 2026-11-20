import PageHead from '../../components/PageHead.tsx'
import { useLook } from '../../theme/theme.ts'
import type { Look } from '../../theme/theme.ts'
import './Settings.css'

const palettes: [Look['palette'], string][] = [
  ['copper', 'Copper'],
  ['catppuccin', 'Catppuccin'],
]

const themes: [Look['theme'], string][] = [
  ['system', 'As the system'],
  ['dark', 'Dark'],
  ['light', 'Light'],
]

function Choice<T extends string>({ id, label, options, value, onChange }: {
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

export default function Settings() {
  const [look, setLook] = useLook()
  return (
    <div className="page">
      <PageHead title="Settings" />
      <div className="set">
        <section>
          <h2>This browser</h2>
          <Choice id="look-palette" label="Palette" options={palettes} value={look.palette} onChange={(palette) => setLook({ ...look, palette })} />
          <Choice id="look-theme" label="Theme" options={themes} value={look.theme} onChange={(theme) => setLook({ ...look, theme })} />
          <p className="sub">Applied at once. It holds for this browser only.</p>
        </section>
      </div>
    </div>
  )
}
