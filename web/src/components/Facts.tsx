import type { Change, Finding, PullRequest, Review } from '../api/types.ts'
import DiffStat from './DiffStat.tsx'
import './Facts.css'

export function PullRequestFacts({ pr }: { pr: PullRequest | null }) {
  if (!pr) return null
  return (
    <section>
      <h3>Pull request</h3>
      <dl className="kv">
        <dt>Number</dt>
        <dd>
          <a href={pr.url} target="_blank" rel="noreferrer">
            {pr.number}
          </a>
          , {pr.state}
        </dd>
        <dt>Checks</dt>
        <dd>
          {pr.checks || 'not read yet'}
          {pr.failed_checks.length > 0 && ': ' + pr.failed_checks.join(', ')}
        </dd>
        <dt>Branch</dt>
        <dd>
          <code>{pr.branch}</code>
        </dd>
        <dt>Head</dt>
        <dd>
          <code>{pr.head.slice(0, 12)}</code>
        </dd>
      </dl>
    </section>
  )
}

export function ChangeFacts({ change }: { change: Change | null }) {
  if (!change) return null
  const { workflows, instructions, large } = change.flagged
  return (
    <section>
      <h3>Change</h3>
      <div>
        {change.files_changed} {change.files_changed === 1 ? 'file' : 'files'} changed, head <code>{change.head_commit.slice(0, 12)}</code>
      </div>
      {[...workflows, ...instructions, ...large].map((p) => (
        <span key={p} className="flag">
          Touches {p}
        </span>
      ))}
      <DiffStat lines={change.diff_stat} />
    </section>
  )
}

function FindingLine({ f }: { f: Finding }) {
  return (
    <p className="finding">
      <span className="sev">{f.severity}</span> <code>{f.where}</code>
      <br />
      {f.what} <span className="why">{f.why}</span>
    </p>
  )
}

export function ReviewFacts({ review }: { review: Review | null }) {
  if (!review) return null
  return (
    <section>
      <h3>Review</h3>
      <div>
        {review.rounds} fix {review.rounds === 1 ? 'round' : 'rounds'}. {review.open.length} open,{' '}
        {review.minor.length} minor.
      </div>
      {review.failed && <div>{review.failed}</div>}
      {[...review.open, ...review.minor].map((f, i) => (
        <FindingLine key={i} f={f} />
      ))}
    </section>
  )
}
