import { render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import type { Change, PullRequest, Review } from '../api/types.ts'
import { ChangeFacts, PullRequestFacts, ReviewFacts } from './Facts.tsx'

const pr: PullRequest = {
  number: 7,
  url: 'https://example.test/pull/7',
  branch: 'cavu/7',
  head: '0123456789abcdef',
  state: 'open',
  checks: 'failed',
  failed_checks: ['lint', 'test'],
  ready: false,
}

it("the pull request's facts link to its address", () => {
  render(<PullRequestFacts pr={pr} />)
  const link = screen.getByRole('link', { name: '7' })
  expect(link).toHaveAttribute('href', pr.url)
  expect(link).toHaveAttribute('target', '_blank')
  expect(link).toHaveAttribute('rel', 'noreferrer')
  expect(screen.getByText('0123456789ab')).toBeInTheDocument()
})

it('a section renders nothing for null', () => {
  const { container } = render(
    <>
      <PullRequestFacts pr={null} />
      <ChangeFacts change={null} />
      <ReviewFacts review={null} />
    </>,
  )
  expect(container).toBeEmptyDOMElement()
})

it('a change names its flagged paths', () => {
  const change: Change = {
    head_commit: 'fedcba9876543210',
    files_changed: 3,
    diff_stat: [' a | 1 +', ' 1 file changed, 1 insertion(+)'],
    flagged: { workflows: ['.github/ci.yaml'], instructions: ['AGENTS.md'], large: ['big.bin'] },
  }
  const { container } = render(<ChangeFacts change={change} />)
  expect([...container.querySelectorAll('.flag')].map((e) => e.textContent)).toEqual([
    'Touches .github/ci.yaml',
    'Touches AGENTS.md',
    'Touches big.bin',
  ])
})

it('a finding with markup is shown as text', () => {
  const finding = (what: string) => ({ severity: 'minor' as const, where: 'x.go:1', what, why: 'because' })
  const review: Review = {
    rounds: 2,
    failed: '',
    open: [finding('<img src=x onerror=alert(1)>')],
    minor: [finding('plain')],
  }
  const { container } = render(<ReviewFacts review={review} />)
  expect(container.querySelector('img')).toBeNull()
  expect(container.querySelectorAll('.finding')).toHaveLength(2)
  expect(container.querySelector('.finding')?.textContent).toContain('<img src=x onerror=alert(1)>')
})
