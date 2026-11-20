import type { Status } from '../api/types.ts'
import './StatusMark.css'

export default function StatusMark({ status }: { status: Status }) {
  return <span className={'st ' + status}>{status.replace('_', ' ')}</span>
}
