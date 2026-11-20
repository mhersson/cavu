import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { Project } from '../api/types.ts'
import { fakeServer, json } from '../test-server.ts'
import EnvCheck from './EnvCheck.tsx'
import { clock } from './time.ts'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const BASE: Project = {
  id: 4,
  name: 'web',
  repo_url: 'https://github.com/acme/web.git',
  base_branch: 'main',
  setup_command: 'npm ci',
  verify_command: 'npm test',
  verify_waived: false,
  tool_overrides: {},
  egress_hosts: [],
  env: {},
  secret_vars: [],
  switches: {},
  caps: {},
  no_ci: false,
  ignored_checks: [],
  review_bots: [],
  forge_credential: '',
  runnable: true,
  baseline: { commit: 'c0ffee1', passed: true, at: '2030-01-02T03:04:05Z', output: '' },
  check: {
    requested_at: null,
    tools: ['node', 'go'],
    ignored_tools: [],
    failed_step: '',
    verify_suggestion: '',
    fault: '',
    commit: 'c0ffee1',
    output: '',
  },
}

function project(more: Partial<Project> = {}, check: Partial<Project['check']> = {}): Project {
  return { ...BASE, ...more, check: { ...BASE.check, ...check } }
}

const failed = project(
  { runnable: false, baseline: { commit: 'badc0de', passed: false, at: '2030-01-02T03:04:05Z', output: '' } },
  { failed_step: 'verify', commit: 'badc0de', output: 'FAIL src/a.test.ts\n  1 failed' },
)

describe('EnvCheck', () => {
  it('a project that cannot run shows its failed step and its output', () => {
    render(<EnvCheck project={failed} onProject={() => {}} />)
    const box = screen.getByRole('region')
    expect(box.classList.contains('k-stop')).toBe(true)
    expect(box.querySelector('.mk')).not.toBeNull()
    expect(screen.getByText('verify').tagName).toBe('CODE')
    expect(screen.getAllByText('badc0de').length).toBeGreaterThan(0)
    const out = box.querySelector('pre.out')
    expect(out?.textContent).toBe('FAIL src/a.test.ts\n  1 failed')
  })

  it('a project that can run shows the baseline it passed on', () => {
    render(<EnvCheck project={BASE} onProject={() => {}} />)
    const box = screen.getByRole('region')
    expect(box.classList.contains('k-ready')).toBe(true)
    expect(screen.getByText('c0ffee1').tagName).toBe('CODE')
    expect(box.textContent).toContain(clock(BASE.baseline!.at))
    expect(box.querySelector('pre.out')).toBeNull()
    expect(box).toHaveTextContent('node, go')
  })

  it('"Check again" asks for a check and shows that one is asked for', async () => {
    const asked = project({}, { requested_at: '2030-01-02T05:06:00Z' })
    const calls = fakeServer((c) => (c.method === 'POST' ? json(asked, 202) : undefined))
    const onProject = vi.fn()
    const { rerender } = render(<EnvCheck project={BASE} onProject={onProject} />)
    await userEvent.click(screen.getByRole('button', { name: 'Check again' }))
    await waitFor(() => expect(onProject).toHaveBeenCalledWith(asked))
    expect(calls).toHaveLength(1)
    expect(calls[0].method).toBe('POST')
    expect(calls[0].path).toBe('/api/projects/4/check')
    expect(calls[0].headers.get('X-CAVU-CSRF')).toBe('1')
    expect(screen.getByRole('region').textContent).not.toContain(clock(asked.check.requested_at!))
    rerender(<EnvCheck project={asked} onProject={onProject} />)
    expect(screen.getByRole('region').textContent).toContain(clock(asked.check.requested_at!))
    expect(screen.getByRole('button', { name: 'Check again' })).toBeEnabled()
  })

  it('a check asked for is read again until it ends', async () => {
    vi.useFakeTimers()
    const asked = project({}, { requested_at: '2030-01-02T05:06:00Z' })
    const ended = project({}, { commit: 'feed123' })
    const calls = fakeServer(() => json(ended))
    const onProject = vi.fn()
    const { rerender, unmount } = render(<EnvCheck project={asked} onProject={onProject} />)
    expect(calls).toHaveLength(0)
    await act(() => vi.advanceTimersByTimeAsync(3000))
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(['GET /api/projects/4'])
    expect(onProject).toHaveBeenCalledWith(ended)

    rerender(<EnvCheck project={ended} onProject={onProject} />)
    await act(() => vi.advanceTimersByTimeAsync(3000))
    expect(calls).toHaveLength(1)

    rerender(<EnvCheck project={asked} onProject={onProject} />)
    await act(() => vi.advanceTimersByTimeAsync(3000))
    expect(calls).toHaveLength(2)
    unmount()
    await act(() => vi.advanceTimersByTimeAsync(9000))
    expect(calls).toHaveLength(2)
  })

  it('a failed read is skipped and the next tick reads again', async () => {
    vi.useFakeTimers()
    const asked = project({}, { requested_at: '2030-01-02T05:06:00Z' })
    let n = 0
    const calls = fakeServer(() => (++n === 1 ? json({ error: 'down' }, 500) : json(asked)))
    const onProject = vi.fn()
    render(<EnvCheck project={asked} onProject={onProject} />)
    await act(() => vi.advanceTimersByTimeAsync(3000))
    expect(onProject).not.toHaveBeenCalled()
    await act(() => vi.advanceTimersByTimeAsync(3000))
    expect(calls).toHaveLength(2)
    expect(onProject).toHaveBeenCalledWith(asked)
  })

  it('the suggested verify command shows only when it differs', () => {
    const { rerender } = render(
      <EnvCheck project={project({}, { verify_suggestion: 'make check' })} onProject={() => {}} />,
    )
    expect(screen.getByText('make check').tagName).toBe('CODE')
    rerender(<EnvCheck project={project({}, { verify_suggestion: 'npm test' })} onProject={() => {}} />)
    expect(screen.queryByText('make check')).toBeNull()
    expect(screen.queryAllByText('npm test')).toHaveLength(0)
    rerender(<EnvCheck project={project({}, { verify_suggestion: '' })} onProject={() => {}} />)
    expect(screen.getByRole('region').querySelectorAll('code')).toHaveLength(1)
  })

  it('tools left out show beside the tools found', () => {
    render(<EnvCheck project={project({}, { ignored_tools: ['ruby'] })} onProject={() => {}} />)
    expect(screen.getByRole('region')).toHaveTextContent('ruby')
  })

  it('a refused check shows the server\'s line', async () => {
    fakeServer(() => json({ error: 'the project is being removed' }, 400))
    const onProject = vi.fn()
    render(<EnvCheck project={BASE} onProject={onProject} />)
    await userEvent.click(screen.getByRole('button', { name: 'Check again' }))
    expect((await screen.findByRole('alert')).textContent).toBe('the project is being removed')
    expect(onProject).not.toHaveBeenCalled()
  })

  it('a check that met an error shows its fault', () => {
    const fault = 'clone failed:\nrepository not found'
    render(<EnvCheck project={project({ runnable: false, baseline: null }, { fault, commit: '' })} onProject={() => {}} />)
    const shown = screen.getByText((_, el) => el?.textContent === fault && el.children.length === 0)
    expect(shown.textContent).toBe(fault)
    expect(screen.getByRole('region').classList.contains('k-stop')).toBe(true)
  })

  it('a project never checked offers "Check"', () => {
    const never = project(
      { runnable: false, baseline: null },
      { tools: [], commit: '', failed_step: '', fault: '', output: '' },
    )
    render(<EnvCheck project={never} onProject={() => {}} />)
    const box = screen.getByRole('region')
    expect(box.querySelector('pre.out')).toBeNull()
    expect(box.querySelectorAll('code')).toHaveLength(0)
    expect(screen.getByRole('button', { name: 'Check' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: 'Check again' })).toBeNull()
  })

  it('a failed clone names no commit', () => {
    const clone = project(
      { runnable: false, baseline: { commit: '', passed: false, at: '2030-01-02T03:04:05Z', output: '' } },
      { failed_step: 'clone', commit: '' },
    )
    render(<EnvCheck project={clone} onProject={() => {}} />)
    const codes = [...screen.getByRole('region').querySelectorAll('code')]
    expect(codes.map((c) => c.textContent)).toEqual(['clone'])
    expect(screen.getByRole('region').textContent).toContain(clock('2030-01-02T03:04:05Z'))
  })

  it('the output is shown as text', () => {
    render(<EnvCheck project={project({}, { failed_step: 'setup', output: '<b>x</b>' })} onProject={() => {}} />)
    const out = screen.getByRole('region').querySelector('pre.out')!
    expect(out.textContent).toBe('<b>x</b>')
    expect(out.querySelector('b')).toBeNull()
  })
})
