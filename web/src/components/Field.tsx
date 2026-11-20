import { cloneElement, useId, type ReactElement } from 'react'
import './Field.css'

type Props = { label: string; check?: boolean; children: ReactElement<{ id?: string }> }

export default function Field({ label, check, children }: Props) {
  const id = useId()
  const control = cloneElement(children, { id })
  const name = <label htmlFor={id}>{label}</label>
  return check ? (
    <div className="field check">
      {control}
      {name}
    </div>
  ) : (
    <div className="field">
      {name}
      {control}
    </div>
  )
}
