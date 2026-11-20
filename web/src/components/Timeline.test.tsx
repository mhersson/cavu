import { render, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { TaskEvent } from '../api/types.ts'
import Timeline from './Timeline.tsx'
import { clock } from './time.ts'

const ev = (id: number, type: string, actor: TaskEvent['actor'] = 'engine', payload: unknown = {}, text = `${type} ${id}`): TaskEvent => ({
  id,
  task_id: 7,
  run_id: 1,
  at: '2030-01-02T03:04:05Z',
  type,
  actor,
  payload,
  text,
})

function show(events: TaskEvent[], read: number[] = []) {
  const onStretch = vi.fn()
  const view = render(<Timeline events={events} read={new Set(read)} onStretch={onStretch} />)
  const again = (more: TaskEvent[], now: number[]) => view.rerender(<Timeline events={more} read={new Set(now)} onStretch={onStretch} />)
  return { onStretch, again, root: view.container.firstElementChild as HTMLElement }
}

const text = (row: Element) => row.querySelector('.tx')!.firstChild!.textContent
// The timeline's top level: a run as "run", a fold of the agent's steps as "fold", a row as its event's text.
const outline = (root: Element) => [...root.children].map((c) => (c.matches('h3') ? 'run' : c.matches('details') ? 'fold' : text(c)))
const folds = (root: Element) => [...root.children].filter((c): c is HTMLDetailsElement => c.matches('details'))
const steps = (fold: Element) => [...fold.querySelectorAll('.ev')].map(text)
const toggle = (d: Element) => userEvent.setup().click(d.querySelector('summary')!)

test("a session's steps fold after its start and the session's other rows stay", () => {
  const { root } = show(
    [
      ev(1, 'run_started'),
      ev(2, 'session_started'),
      ev(3, 'agent_tool', 'agent'),
      ev(4, 'egress_denied', 'runner'),
      ev(5, 'agent_text', 'agent'),
      ev(6, 'human_cancelled', 'user'),
      ev(7, 'agent_tool', 'agent'),
      ev(8, 'session_ended', 'runner'),
      ev(9, 'stopped'),
    ],
    [2],
  )
  expect(outline(root)).toEqual(['run', 'session_started 2', 'fold', 'egress_denied 4', 'human_cancelled 6', 'session_ended 8', 'stopped 9'])
  expect(steps(folds(root)[0])).toEqual(['agent_tool 3', 'agent_text 5', 'agent_tool 7'])
})

test('a stop or a new start ends a session', async () => {
  const events = [
    ev(1, 'run_started'),
    ev(2, 'session_started'),
    ev(3, 'agent_tool', 'agent'),
    ev(4, 'stopped'),
    ev(5, 'run_started'),
    ev(6, 'review_started'),
    ev(7, 'agent_text', 'agent'),
    ev(8, 'session_started'),
    ev(9, 'agent_tool', 'agent'),
    ev(10, 'session_ended', 'runner'),
  ]
  const { root, onStretch, again } = show(events, [2, 6, 8])
  expect(outline(root)).toEqual(['run', 'session_started 2', 'fold', 'stopped 4', 'run', 'review_started 6', 'fold', 'session_started 8', 'fold', 'session_ended 10'])
  expect(folds(root).map(steps)).toEqual([['agent_tool 3'], ['agent_text 7'], ['agent_tool 9']])
  expect(folds(root).map((d) => d.open)).toEqual([false, false, false])

  again(events, [])
  for (const d of folds(root)) await toggle(d)
  expect(onStretch.mock.calls).toEqual([
    [2, 4],
    [6, 8],
    [8, 10],
  ])
})

test('a fold counts its steps only once its stretch is read', () => {
  const events = [ev(1, 'session_started'), ev(2, 'agent_tool', 'agent'), ev(3, 'agent_text', 'agent'), ev(4, 'session_ended', 'runner'), ev(5, 'session_started'), ev(6, 'session_ended', 'runner')]
  const { root, again } = show(events)
  const counts = () => folds(root).map((d) => d.querySelector('summary')!.textContent!.match(/^\d+ /)?.[0])
  expect(counts()).toEqual([undefined, undefined])
  again(events, [1, 5])
  expect(counts()).toEqual(['2 ', '0 '])
})

test("a fold's end has its day only when it differs from the start's", () => {
  const at = (id: number, type: string, iso: string) => ({ ...ev(id, type), at: iso })
  const start = '2020-03-04T12:00:00Z'
  const sameDay = '2020-03-04T12:05:00Z'
  const later = '2020-03-06T12:00:00Z'
  const { root } = show([at(1, 'session_started', start), at(2, 'session_ended', sameDay), at(3, 'session_started', start), at(4, 'session_ended', later)])
  const [one, two] = folds(root).map((d) => d.querySelector('summary')!.textContent!)
  const startClock = clock(start)
  const day = startClock.slice(0, startClock.lastIndexOf(' '))
  const time = (iso: string) => clock(iso).slice(clock(iso).lastIndexOf(' ') + 1)

  expect(one).toContain(startClock)
  expect(one.split(day)).toHaveLength(2)
  expect(one.endsWith(time(sameDay))).toBe(true)
  expect(two).toContain(startClock)
  expect(two.endsWith(clock(later))).toBe(true)
})

test('the live fold is open and asks for its stretch', () => {
  const events = [ev(1, 'session_started'), ev(2, 'session_ended', 'runner'), ev(3, 'session_started'), ev(4, 'agent_tool', 'agent')]
  const { root, onStretch, again } = show(events)
  expect(folds(root).map((d) => d.open)).toEqual([false, true])
  expect(onStretch.mock.calls).toEqual([[3, undefined]])
  expect(folds(root)[1].querySelector('summary')).not.toHaveTextContent(/^\d/)

  // its steps are read; then its session ends, and the fold no one touched closes
  again(events, [3])
  again([...events, ev(5, 'session_ended', 'runner')], [3])
  expect(folds(root).map((d) => d.open)).toEqual([false, false])
  expect(onStretch).toHaveBeenCalledTimes(1)
})

test('opening a fold asks for its stretch once', async () => {
  const events = [ev(1, 'session_started'), ev(2, 'agent_tool', 'agent'), ev(3, 'session_ended', 'runner'), ev(4, 'verify_started')]
  const { root, onStretch, again } = show(events)
  const [fold] = folds(root)
  expect(onStretch).not.toHaveBeenCalled()

  await toggle(fold)
  expect(fold.open).toBe(true)
  expect(onStretch.mock.calls).toEqual([[1, 3]])

  again(events, [1])
  await toggle(fold)
  expect(fold.open).toBe(false)
  await toggle(fold)
  expect(fold.open).toBe(true)
  expect(onStretch).toHaveBeenCalledTimes(1)
})

const verify = (id: number, result: string, output = `output ${id}`) =>
  ev(id, 'verify_ended', 'runner', { result, command: 'npm test', exit_code: result === 'passed' ? 0 : 1, output, duration_ms: 218000 })

test("a failed verify's output is open and a passed one is closed", async () => {
  const { root } = show([verify(1, 'failed'), verify(2, 'timeout'), verify(3, 'passed')])
  const shown = [...root.querySelectorAll('details')]
  expect(shown.map((d) => d.open)).toEqual([true, true, false])
  expect(shown.map((d) => d.querySelector('pre')!.textContent)).toEqual(['output 1', 'output 2', 'output 3'])
  expect(within(shown[0].querySelector('summary')!).getByText('npm test').tagName).toBe('CODE')

  await toggle(shown[2])
  expect(shown[2].open).toBe(true)
  await toggle(shown[0])
  expect(shown[0].open).toBe(false)
})

test("a failed preparation's output is open in its box", () => {
  const output = 'npm ci\nnpm ERR! code ERESOLVE\n  peer react@"^18" from a@1.0.0'
  const { root } = show([
    ev(1, 'prepare_failed', 'runner', { step: 'setup', output }),
    ev(2, 'prepare_failed', 'runner', { step: 'clone', output: '' }),
  ])
  const shown = [...root.querySelectorAll('details')]
  expect(shown).toHaveLength(1)
  expect(shown[0].open).toBe(true)
  const boxes = root.querySelectorAll('pre')
  expect(boxes).toHaveLength(1)
  expect(boxes[0].textContent).toBe(output)
  expect(text(boxes[0].closest('.ev')!)).toBe('prepare_failed 1')
})

test('a long output stays in its box', () => {
  const output = 'FAIL src/a.test.ts > a long line of output\n'.repeat(24000)
  expect(output.length).toBeGreaterThan(1e6)
  const { root } = show([ev(1, 'verify_started'), verify(2, 'failed', output), ev(3, 'stopped')])
  const boxes = root.querySelectorAll('pre')
  expect(boxes).toHaveLength(1)
  expect(boxes[0].textContent).toBe(output)
  expect(boxes[0].childElementCount).toBe(0)
  expect(text(boxes[0].closest('.ev')!)).toBe('verify_ended 2')
})

test('markup in an event is shown as text', () => {
  const said = '<b>bold</b>'
  const { root } = show([
    ev(1, 'prepared', 'runner', {}, '<img src=x onerror=alert(1)>'),
    verify(2, 'failed', '<script>alert(1)</script>'),
    ev(3, 'session_ended', 'runner', { summary: said, assumptions: ['<i>it</i>'], where_to_look: ['<a href="x">y</a>'] }),
  ])
  expect(root.querySelector('img, script, b, i, a')).toBeNull()
  expect(text(root.children[0])).toBe('<img src=x onerror=alert(1)>')
  expect(root.querySelector('pre')!.textContent).toBe('<script>alert(1)</script>')
  expect(root.querySelector('[role="note"]')).toHaveTextContent(said)
  expect(root.querySelector('[role="note"]')).toHaveTextContent('<i>it</i>')
  expect(root.querySelector('[role="note"]')).toHaveTextContent('<a href="x">y</a>')
})

test("the agent's own account is labelled as the agent's", async () => {
  const { root } = show([
    ev(1, 'session_ended', 'runner', { summary: 'One line.\nAnother line.', assumptions: ['the footer is global'], where_to_look: ['a.go', 'b.go'] }),
    ev(2, 'session_ended', 'runner', { summary: 'Short.', assumptions: null }),
    ev(3, 'session_ended', 'runner', { summary: '', assumptions: ['unseen'], where_to_look: ['unseen.go'] }),
  ])
  const shown = [...root.querySelectorAll('details')]
  expect(shown).toHaveLength(2)
  expect(shown.map((d) => d.open)).toEqual([false, false])
  for (const d of shown) await toggle(d)
  expect(shown.map((d) => d.open)).toEqual([true, true])

  const notes = within(root).getAllByRole('note')
  expect(notes).toHaveLength(2)
  notes.forEach((n, i) => expect(n).toHaveAccessibleName(shown[i].querySelector('summary')!.textContent!))
  expect(notes[0]).toHaveTextContent('One line.')
  expect([...notes[0].querySelector('p')!.childNodes].map((n) => n.nodeName)).toEqual(['#text', 'BR', '#text'])
  expect(within(notes[0]).getAllByRole('listitem').map((li) => li.textContent)).toEqual([
    expect.stringContaining('the footer is global'),
    expect.stringContaining('a.go'),
    expect.stringContaining('b.go'),
  ])
  expect(within(notes[1]).queryAllByRole('listitem')).toHaveLength(0)
})

test('an agent line renders inline code', () => {
  const { root } = show([ev(1, 'agent_text', 'agent', {}, 'Read `a.go` and `b.go` first.')])
  expect([...root.querySelectorAll('.ev code')].map((c) => c.textContent)).toEqual(['a.go', 'b.go'])
  expect(root.querySelector('.ev')!.textContent).not.toContain('`')
})

test('verify output stays text', () => {
  const said = '# FAIL\n- `a.test.ts` **failed**, see https://example.test/log'
  const { root } = show([verify(1, 'failed', said), ev(2, 'prepare_failed', 'runner', { step: 'setup', output: said }), ev(3, 'stopped', 'engine', {}, said), ev(4, 'agent_tool', 'agent', {}, 'Bash ls **/*.go `x`')])
  expect([...root.querySelectorAll('pre')].map((b) => [b.textContent, b.childElementCount])).toEqual([
    [said, 0],
    [said, 0],
  ])
  expect(text(root.children[2])).toBe(said)
  expect(root.querySelector('h2, h3, h4, ul, strong, a, br')).toBeNull()
  const tool = within(root).getByText('Bash ls **/*.go `x`')
  expect(tool.childElementCount).toBe(0)
  expect(tool.firstChild!.nodeType).toBe(Node.TEXT_NODE)
})

test("the account's summary is Markdown, and each listed line takes the inline part after its lead", () => {
  const { root } = show([
    ev(1, 'session_ended', 'runner', { summary: 'Done:\n\n- `a.go`\n- `b.go`', assumptions: ['`x` is **global**'], where_to_look: ['`c.go`: the check'] }),
  ])
  const note = within(root).getByRole('note')
  expect([...note.firstElementChild!.querySelectorAll('li > code')].map((c) => c.textContent)).toEqual(['a.go', 'b.go'])
  const lines = [...note.querySelectorAll(':scope > ul > li')]
  expect(lines.map((li) => [li.firstChild!.nodeType, li.querySelector('code')!.textContent])).toEqual([
    [Node.TEXT_NODE, 'x'],
    [Node.TEXT_NODE, 'c.go'],
  ])
  expect(lines[0].querySelector('strong')!.textContent).toBe('global')
})

test('runs are numbered and agent, stop and person rows carry their marks', () => {
  const { root } = show(
    [
      ev(1, 'task_started', 'user'),
      ev(2, 'run_started', 'engine', {}, 'a first try'),
      ev(3, 'session_started'),
      ev(4, 'agent_text', 'agent'),
      ev(5, 'session_ended', 'runner'),
      ev(6, 'stopped'),
      ev(7, 'run_ended'),
      ev(8, 'run_started', 'engine', {}, 'a second try'),
    ],
    [3],
  )
  const runs = within(root).getAllByRole('heading', { level: 3 })
  expect(runs).toHaveLength(2)
  expect(runs[0]).toHaveTextContent(/1\D*a first try$/)
  expect(runs[1]).toHaveTextContent(/2\D*a second try$/)

  const marks = (t: string) => [...root.querySelectorAll('.ev')].find((r) => text(r) === t)!.className.split(' ').slice(1)
  expect(marks('task_started 1')).toEqual(['user'])
  expect(marks('agent_text 4')).toEqual(['agent'])
  expect(marks('stopped 6')).toEqual(['stopped'])
  expect(marks('session_ended 5')).toEqual([])
})
