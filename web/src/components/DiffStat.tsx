import type { ReactNode } from 'react'
import './DiffStat.css'

// Splits text on a pattern with one capture group and marks each captured run.
function mark(text: string, pattern: RegExp, cls: (run: string) => string): ReactNode[] {
  return text.split(pattern).map((part, i) => (i % 2 ? <span key={i} className={cls(part)}>{part}</span> : part))
}

const runs = /(\++|-+)/
const totals = /(\d+ insertions?\(\+\)|\d+ deletions?\(-\))/

function StatLine({ line }: { line: string }) {
  const i = line.lastIndexOf('|')
  if (i < 0) return line
  return (
    <>
      {line.slice(0, i + 1)}
      {mark(line.slice(i + 1), runs, (r) => (r[0] === '+' ? 'add' : 'del'))}
    </>
  )
}

// lines is change.diff_stat as served: a line per file, then the totals.
export default function DiffStat({ lines }: { lines: string[] }) {
  if (lines.length === 0) return null
  const files = lines.slice(0, -1)
  return (
    <>
      <div className="stat">
        {files.map((l, i) => (
          <span key={i}>
            <StatLine line={l} />
            {i < files.length - 1 && '\n'}
          </span>
        ))}
      </div>
      <div className="stat-totals">
        {mark(lines[lines.length - 1].trim(), totals, (r) => (r.endsWith('(+)') ? 'add' : 'del'))}
      </div>
    </>
  )
}
