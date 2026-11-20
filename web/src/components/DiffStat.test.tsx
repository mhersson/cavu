import { render } from '@testing-library/react'
import { expect, it } from 'vitest'
import DiffStat from './DiffStat.tsx'

const lines = [
  ' a.go | 5 +++--',
  ' b.go | 2 ++',
  ' 2 files changed, 4 insertions(+), 2 deletions(-)',
]

it('additions and deletions are marked in each line and in the totals', () => {
  const { container } = render(<DiffStat lines={lines} />)
  const add = [...container.querySelectorAll('.add')].map((e) => e.textContent)
  const del = [...container.querySelectorAll('.del')].map((e) => e.textContent)
  expect(add).toEqual(['+++', '++', '4 insertions(+)'])
  expect(del).toEqual(['--', '2 deletions(-)'])
})

it('markup in a path is shown as text', () => {
  const { container } = render(<DiffStat lines={[' <b>x</b>.go | 1 +', ' 1 file changed, 1 insertion(+)']} />)
  expect(container.querySelector('b')).toBeNull()
  expect(container.textContent).toContain('<b>x</b>.go')
})

it('an empty list renders nothing', () => {
  const { container } = render(<DiffStat lines={[]} />)
  expect(container).toBeEmptyDOMElement()
})
