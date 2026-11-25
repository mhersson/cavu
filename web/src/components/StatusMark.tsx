import type { ReactNode } from 'react'
import type { Status } from '../api/types.ts'
import './StatusMark.css'

export default function StatusMark({ status, children }: { status: Status; children?: ReactNode }) {
  return <span className={'st ' + status}>{children}<span>{status.replace('_', ' ')}</span></span>
}
