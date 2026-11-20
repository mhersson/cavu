import type { Project, Row, Settings, Task, TaskEvent, Usage } from '../api/types.ts'

// What the API serves, in its shapes; the harness answers from it. A screen's own
// states are added here as the screen is built.
export const HEALTH = { status: 'ok', version: '0.4.0' };                 // GET /api/health
// GET /api/projects
const SWITCHES: Record<string, boolean> = { continuations: true, publish: true, readiness: true, review: true, verify_fix: true, web_tools: false };
export const PROJECTS: Project[] = [
  { id: 1, name: 'cavu', repo_url: 'https://github.com/mhersson/cavu.git', base_branch: 'main', setup_command: 'go mod download', verify_command: 'make check', verify_waived: false,
    tool_overrides: {}, egress_hosts: ['proxy.golang.org', 'sum.golang.org'], env: { CGO_ENABLED: '0' }, secret_vars: [], switches: SWITCHES,
    caps: { task_timeout_minutes: '90' }, no_ci: false, ignored_checks: ['macos'], review_bots: [], forge_credential: '', runnable: true,
    baseline: { commit: '934b98c1d2e3', passed: true, at: '2026-11-19T15:58:00Z', output: '' },
    check: { requested_at: null, started_at: null, tools: ['go', 'golangci-lint'], ignored_tools: [], failed_step: '', verify_suggestion: 'make check', fault: '', commit: '934b98c1d2e3', output: '' } },
  { id: 2, name: 'homelander', repo_url: 'https://github.com/mhersson/homelander.git', base_branch: 'main', setup_command: 'npm ci', verify_command: 'npm test', verify_waived: false,
    tool_overrides: { node: '24' }, egress_hosts: ['registry.npmjs.org'], env: { CI: '1' }, secret_vars: [], switches: SWITCHES,
    caps: {}, no_ci: false, ignored_checks: [], review_bots: ['copilot[bot]'], forge_credential: '', runnable: true,
    baseline: { commit: '0e41aa7c95d2', passed: true, at: '2026-11-19T14:40:00Z', output: '' },
    check: { requested_at: '2026-11-19T16:30:00Z', started_at: '2026-11-19T16:31:10Z', tools: ['node'], ignored_tools: [], failed_step: '', verify_suggestion: 'npm test', fault: '', commit: '0e41aa7c95d2', output: '' } },
  { id: 3, name: 'docs-site', repo_url: 'https://github.com/acme/docs-site.git', base_branch: 'main', setup_command: 'npm ci', verify_command: 'npm run build', verify_waived: false,
    tool_overrides: {}, egress_hosts: ['registry.npmjs.org', 'npm.pkg.github.com'], env: {}, secret_vars: [], switches: { ...SWITCHES, review: false },
    caps: {}, no_ci: true, ignored_checks: [], review_bots: [], forge_credential: 'acme-org', runnable: false, baseline: null,
    check: { requested_at: null, started_at: null, tools: ['node'], ignored_tools: ['ruby'], failed_step: 'setup', verify_suggestion: 'npm test', fault: '', commit: '77ab01c4de90',
      output: 'npm error code E401\nnpm error Unable to authenticate, need: Basic realm="GitHub Package Registry"\nnpm error A complete log of this run can be found in: /home/agent/.npm/_logs/2026-11-19T18_02_11_301Z-debug-0.log' } },
];
// GET /api/settings: only the keys that are set, and whether each secret is
export const SETTINGS: Settings = { settings: { endpoint_mode: 'subscription', model_work: 'sonnet', effort_work: 'high', model_review: 'sonnet', model_strong: 'opus', concurrency: '2', commit_name: 'cavu', commit_email: 'cavu@example.com' },
  secrets: { endpoint_credential: true, forge_token: true, forge_app_key: false },
  defaults: { endpoint_mode: 'subscription', model_work: 'sonnet', effort_work: 'high', model_review: 'sonnet', model_strong: 'opus', concurrency: '2', commit_name: '', commit_email: '' },
  switches: SWITCHES, forge_credentials: [] };

// GET /api/tasks, oldest first
export const ROWS: Row[] = [
  { id: 29, project: 'homelander', title: 'Try the new router', status: 'cancelled', needs_you: false, needs: '', needs_you_since: null, stop_reason: 'cancelled', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-18T09:12:00Z' },
  { id: 30, project: 'cavu', title: 'Print the view of a task', status: 'done', needs_you: false, needs: '', needs_you_since: null, stop_reason: '', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-18T11:40:00Z' },
  { id: 31, project: 'cavu', title: 'Check the image against the binary', status: 'done', needs_you: false, needs: '', needs_you_since: null, stop_reason: '', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-18T15:03:00Z' },
  { id: 32, project: 'homelander', title: 'Sort the board by age', status: 'done', needs_you: false, needs: '', needs_you_since: null, stop_reason: '', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T08:30:00Z' },
  { id: 33, project: 'homelander', title: "Rename the settings page's sections", status: 'in_review', needs_you: true, needs: 'take', needs_you_since: '2026-11-19T13:41:00Z', stop_reason: '', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T13:05:00Z' },
  { id: 35, project: 'cavu', title: "Tell a rate limit's reset in local time", status: 'blocked', needs_you: true, needs: 'retry', needs_you_since: '2026-11-19T15:38:00Z', stop_reason: 'ci_failed', waiting: 'The task is stopped until a person acts; its pull request is watched for a merge or a close.', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T14:20:00Z' },
  { id: 36, project: 'homelander', title: 'Bump the Node toolchain to 24', status: 'blocked', needs_you: true, needs: 'retry', needs_you_since: '2026-11-19T15:52:00Z', stop_reason: 'verify_failed', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T15:11:00Z' },
  { id: 38, project: 'cavu', title: "Show the image's Claude Code version in the doctor", status: 'in_review', needs_you: true, needs: 'review', needs_you_since: '2026-11-19T16:41:00Z', stop_reason: '', waiting: 'The pull request waits for a person to merge it.', waiting_until: null, pull_request_ready: true, created_at: '2026-11-19T16:02:00Z' },
  { id: 39, project: 'homelander', title: 'Add a health badge to the README', status: 'in_review', needs_you: false, needs: '', needs_you_since: null, stop_reason: '', waiting: "The pull request's checks are running.", waiting_until: '2026-11-19T21:31:00Z', pull_request_ready: false, created_at: '2026-11-19T16:30:00Z' },
  { id: 40, project: 'homelander', title: 'Split the router into one file per page', status: 'blocked', needs_you: false, needs: '', needs_you_since: null, stop_reason: 'usage_limit', waiting: 'The usage limit has not reset yet.', waiting_until: '2026-11-19T22:10:00Z', pull_request_ready: false, created_at: '2026-11-19T16:44:00Z' },
  { id: 41, project: 'homelander', title: 'Add a footer with the build version', status: 'blocked', needs_you: true, needs: 'answer', needs_you_since: '2026-11-19T17:05:00Z', stop_reason: 'question', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T17:02:00Z' },
  { id: 42, project: 'cavu', title: "Serve the web app's files from the binary", status: 'running', needs_you: false, needs: '', needs_you_since: null, stop_reason: '', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T19:21:00Z' },
  { id: 43, project: 'cavu', title: 'Add a login page', status: 'queued', needs_you: false, needs: '', needs_you_since: null, stop_reason: '', waiting: 'Task 42, which this task depends on, is not done.', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T19:22:00Z' },
  { id: 44, project: 'cavu', title: 'Write the web app into the README', status: 'draft', needs_you: false, needs: '', needs_you_since: null, stop_reason: '', waiting: '', waiting_until: null, pull_request_ready: false, created_at: '2026-11-19T19:30:00Z' },
];

const usage = (model: string, input: number, output: number, cache_read: number, cache_write: number, cost: number): Usage => ({ source: 'agent', model, input, output, cache_read, cache_write, cost_estimate_usd: cost });
const base = (id: number, more: Partial<Task>): Task => {
  const r = ROWS.find((x) => x.id === id)!
  return { id, project: r.project, title: r.title, body: '', requester: 'api', created_at: r.created_at, status: r.status, needs_you: r.needs_you, needs: r.needs, needs_you_since: r.needs_you_since,
    waiting: r.waiting, waiting_until: r.waiting_until, dependencies: [], stop: null, questions: [], review: null, pull_request: null, change: null,
    turns: 0, duration_ms: 0, cost_estimate_usd: 0, cost_missing_sessions: 0, usage: [], budget: { turns: 0, max_turns: 1000, minutes: 0, max_minutes: 300 } , ...more }
}

// GET /api/tasks/{id}
export const VIEWS: Record<number, Task> = {
  33: base(33, {
    body: 'Group the settings under "Model", "GitHub" and "Limits", in that order.', requester: 'cli',
    change: { head_commit: '51f0c2a9d77e41b0a6c3', files_changed: 3, diff_stat: [' web/src/pages/Settings.tsx      | 64 ++++++++++++-----------', ' web/src/pages/Settings.test.tsx | 21 +++++--', ' web/src/i18n/en.json            |  9 ++-', ' 3 files changed, 55 insertions(+), 39 deletions(-)'], flagged: { workflows: [], instructions: [], large: [] } },
    review: { rounds: 0, failed: '', open: [], minor: [] },
    turns: 27, duration_ms: 548000, cost_estimate_usd: 0.61, usage: [usage('claude-sonnet-5-5', 30100, 15800, 702000, 61000, 0.61)], budget: { turns: 27, max_turns: 1000, minutes: 8, max_minutes: 300 },
  }),
  35: base(35, {
    body: 'The stop for a usage limit prints the reset in UTC. Print it in the server\'s local time, as the wait line does.',
    stop: { reason: 'ci_failed', explanation: 'Checks failed on pull request https://github.com/mhersson/cavu/pull/56: go-checks.', action: 'retry', resets_at: null },
    pull_request: { number: 56, url: 'https://github.com/mhersson/cavu/pull/56', branch: 'cavu/tell-a-rate-limits-reset-in-local-time', head: '7d02b1e4c9a8f3306e15', state: 'open', checks: 'failed', failed_checks: ['go-checks'], ready: false },
    change: { head_commit: '7d02b1e4c9a8f3306e15', files_changed: 2, diff_stat: [' internal/engine/tell.go      | 12 +++---', ' internal/engine/tell_test.go | 30 ++++++++++', ' 2 files changed, 36 insertions(+), 6 deletions(-)'], flagged: { workflows: [], instructions: [], large: [] } },
    review: { rounds: 0, failed: '', open: [], minor: [] },
    turns: 71, duration_ms: 4680000, cost_estimate_usd: 2.07, usage: [usage('claude-sonnet-5-5', 88000, 51200, 2410000, 190000, 2.07)], budget: { turns: 71, max_turns: 1000, minutes: 31, max_minutes: 300 },
  }),
  36: base(36, {
    body: 'Move .tool-versions and the CI matrix to Node 24, and fix what breaks.', requester: 'mcp',
    stop: { reason: 'verify_failed', explanation: 'The change did not pass the verify command.', action: 'retry', resets_at: null },
    turns: 64, duration_ms: 2460000, cost_estimate_usd: 1.38, usage: [usage('claude-sonnet-5-5', 70400, 39900, 1630000, 141000, 1.38)], budget: { turns: 64, max_turns: 1000, minutes: 29, max_minutes: 300 },
  }),
  38: base(38, {
    body: '## Goal\n\nThe doctor prints the Claude Code version the binary pins. Make it also read the version the image was built with, and say when the two differ.\n\n' +
      '## Acceptance\n\n- `cavu doctor` reads the version from the image\'s label\n  - one inspect call gives both values\n  - a missing label means an image built by an older cavu\n' +
      '- A difference is a warning, **not a failure**\n- `docs/cli.md` says what the check prints\n\n### Where the label is set\n\n' +
      '```go\nlabels["io.cavu.claude-code"] = version\n```\n\nThe check is described in [docs/cli.md](https://github.com/mhersson/cavu/blob/main/docs/cli.md).',
    waiting: 'The pull request waits for a person to merge it.', dependencies: [{ id: 31, status: 'done' }],
    pull_request: { number: 57, url: 'https://github.com/mhersson/cavu/pull/57', branch: 'cavu/show-the-images-claude-code-version-in-the-doctor', head: '9c1e44d07ab2f1e05b38', state: 'open', checks: 'passed', failed_checks: [], ready: true },
    change: { head_commit: '9c1e44d07ab2f1e05b38', files_changed: 6, diff_stat: [' .github/workflows/ci.yaml      |  2 +-', ' README.md                      |  3 +', ' cmd/cavu/doctor.go             | 41 +++++++++++---', ' docs/cli.md                    |  6 +-', ' internal/sandbox/image.go      | 18 ++++++', ' internal/sandbox/image_test.go | 52 +++++++++++++++++', ' 6 files changed, 108 insertions(+), 14 deletions(-)'], flagged: { workflows: ['.github/workflows/ci.yaml'], instructions: [], large: [] } },
    review: { rounds: 1, failed: '', open: [], minor: [{ severity: 'minor', where: 'cmd/cavu/doctor.go:88', what: 'The image label is read twice.', why: 'One inspect call would give both values.' }] },
    turns: 58, duration_ms: 2330000, cost_estimate_usd: 1.84,
    usage: [usage('claude-sonnet-5-5', 61000, 42000, 1930000, 148000, 1.71), usage('claude-haiku-5-5', 18200, 6100, 240000, 22000, 0.13)],
    budget: { turns: 58, max_turns: 1000, minutes: 17, max_minutes: 300 },
  }),
  41: base(41, {
    body: 'Show the version from /api/health in the footer of every page, with a link to the changelog.', requester: 'mcp',
    stop: { reason: 'question', explanation: 'Which pages should the footer link to?', action: 'answer', resets_at: null },
    questions: [
      { question: 'Which pages should the footer link to?', suggested_answer: 'The changelog and the repository, as the header does.' },
      { question: 'Should the login page show the version too, before anybody is logged in?', suggested_answer: 'Yes: the health route needs no login.' },
    ],
    turns: 6, duration_ms: 178000, cost_estimate_usd: 0.04, usage: [usage('claude-haiku-5-5', 9100, 2300, 64000, 12000, 0.04)], budget: { turns: 6, max_turns: 1000, minutes: 2, max_minutes: 300 },
  }),
  42: base(42, {
    body: 'Embed the built web app and serve it from cavu serve, with the single page as the fallback for a path that is no file.',
    turns: 18, duration_ms: 1130000, cost_estimate_usd: 0.42, usage: [usage('claude-sonnet-5-5', 22000, 9800, 410000, 52000, 0.42)], budget: { turns: 18, max_turns: 1000, minutes: 16, max_minutes: 300 },
  }),
};

// GET /api/tasks/{id}/events: each line is the event's at, type, actor, text and what of its payload a screen reads
let evId = 4000
const ev = (task_id: number, run_id: number | null, at: string, type: string, actor: TaskEvent['actor'], text: string, payload?: unknown): TaskEvent => ({ id: ++evId, task_id, run_id, at: '2026-11-19T' + at + 'Z', type, actor, payload: payload || {}, text });
const steps = (task: number, run: number, list: [string, string, string][]) => list.map(([at, tool, s]) => tool ? ev(task, run, at, 'agent_tool', 'agent', tool + ' ' + s) : ev(task, run, at, 'agent_text', 'agent', s));
export const EVENTS: Record<number, TaskEvent[]> = {
  38: [
    ev(38, null, '16:02:10', 'task_started', 'user', 'the task is started'),
    ev(38, 61, '16:02:10', 'run_started', 'engine', "the task's first run"),
    ev(38, 61, '16:02:11', 'prepare_started', 'engine', 'The run is not prepared.'),
    ev(38, 61, '16:03:02', 'prepared', 'runner', 'prepared on base commit 934b98c1d2e3; the baseline verify passed'),
    ev(38, 61, '16:03:02', 'session_started', 'engine', 'readiness'),
    ev(38, 61, '16:03:40', 'session_ended', 'runner', 'ready after 3 turns'),
    ev(38, 61, '16:03:41', 'session_started', 'engine', 'start'),
    ...steps(38, 61, [
      ['16:03:52', 'Read', 'cmd/cavu/doctor.go'],
      ['16:04:05', '', 'The doctor prints the version the binary pins and never asks the image.'],
      ['16:04:20', 'Grep', 'claude-code in internal/sandbox'],
      ['16:04:41', 'Read', 'internal/sandbox/image.go'],
      ['16:05:30', '', 'The image carries the version as the label `io.cavu.claude-code`. I will read it with one inspect and compare.'],
      ['16:06:12', 'Edit', 'internal/sandbox/image.go'],
      ['16:07:48', 'Edit', 'cmd/cavu/doctor.go'],
      ['16:09:03', 'Write', 'internal/sandbox/image_test.go'],
      ['16:10:20', 'Bash', 'go test ./internal/sandbox/ ./cmd/cavu/'],
      ['16:11:36', 'Edit', 'cmd/cavu/doctor.go'],
      ['16:12:10', 'Bash', 'make check'],
      ['16:14:30', 'Edit', 'docs/cli.md'],
      ['16:15:02', 'Edit', '.github/workflows/ci.yaml'],
      ['16:15:20', 'Bash', 'git commit'],
    ]),
    ev(38, 61, '16:15:41', 'session_ended', 'runner', 'done after 41 turns', { summary: 'The doctor now reads the Claude Code version from the image\'s label and says when it differs from the binary\'s pin.\n\n- A missing label is reported as an image built by an older cavu.\n- `docs/cli.md` shows the new line.', assumptions: ['An image without the label is an old image, not a broken one.'], where_to_look: ['`cmd/cavu/doctor.go`: the comparison and its two messages', '.github/workflows/ci.yaml: the build step now passes the version'] }),
    ev(38, 61, '16:15:42', 'verify_started', 'engine', 'The change has not been verified.'),
    ev(38, 61, '16:17:55', 'verify_ended', 'runner', 'passed', { result: 'passed', command: 'make check', exit_code: 0, duration_ms: 133000, output: 'go build ./...\ngolangci-lint run\n0 issues.\nok  \tgithub.com/mhersson/cavu/cmd/cavu\t2.411s\nok  \tgithub.com/mhersson/cavu/internal/sandbox\t1.204s\n...\nno em-dash found\n24911 non-test Go lines' }),
    ev(38, 61, '16:17:56', 'review_started', 'engine', 'the whole change'),
    ev(38, 61, '16:19:30', 'review_ended', 'runner', 'reviewed after 9 turns with 2 findings'),
    ev(38, 61, '16:19:31', 'session_started', 'engine', 'fix_review'),
    ...steps(38, 61, [
      ['16:19:50', 'Read', 'cmd/cavu/doctor.go'],
      ['16:20:31', 'Edit', 'cmd/cavu/doctor.go'],
      ['16:21:02', 'Bash', 'go test ./cmd/cavu/'],
    ]),
    ev(38, 61, '16:21:20', 'session_ended', 'runner', 'done after 5 turns', { summary: 'The doctor no longer reports a difference when the image has no label; it says the image is older than the check.', assumptions: [], where_to_look: ['cmd/cavu/doctor.go:71'] }),
    ev(38, 61, '16:21:21', 'verify_started', 'engine', 'The change has not been verified.'),
    ev(38, 61, '16:23:30', 'verify_ended', 'runner', 'passed', { result: 'passed', command: 'make check', exit_code: 0, duration_ms: 129000, output: 'go build ./...\ngolangci-lint run\n0 issues.\nok  \tgithub.com/mhersson/cavu/cmd/cavu\t2.398s\n...\n24914 non-test Go lines' }),
    ev(38, 61, '16:23:31', 'review_started', 'engine', 'the fix and the findings it was to address'),
    ev(38, 61, '16:24:40', 'review_ended', 'runner', 'reviewed after 4 turns with 1 finding; 1 of the 1 open ones addressed'),
    ev(38, 61, '16:24:41', 'export_started', 'engine', 'The change passed verify, or verify is waived.'),
    ev(38, 61, '16:24:52', 'exported', 'runner', 'exported 6 changed files, head 9c1e44d07ab2'),
    ev(38, 61, '16:24:52', 'publish_started', 'engine', "The exported change is not yet on the task's branch on GitHub."),
    ev(38, 61, '16:25:04', 'published', 'runner', 'pull request https://github.com/mhersson/cavu/pull/57 on branch cavu/show-the-images-claude-code-version-in-the-doctor'),
    ev(38, 61, '16:25:04', 'run_ended', 'engine', 'the run ended: exported'),
    ev(38, null, '16:25:15', 'pr_observed', 'runner', 'open, a draft; checks pending; 0 open requests of reviewers'),
    ev(38, null, '16:40:22', 'pr_observed', 'runner', 'open, a draft; checks passed; 0 open requests of reviewers'),
    ev(38, null, '16:40:22', 'ready_started', 'engine', "The pull request's checks passed, or its project has no CI, so it is ready for review."),
    ev(38, null, '16:40:25', 'pr_ready', 'runner', 'the pull request is marked ready for review'),
    ev(38, null, '16:41:26', 'pr_observed', 'runner', 'open; checks passed; 0 open requests of reviewers'),
  ],
  36: [
    ev(36, null, '15:11:30', 'task_started', 'user', 'the task is started'),
    ev(36, 58, '15:11:30', 'run_started', 'engine', "the task's first run"),
    ev(36, 58, '15:11:31', 'prepare_started', 'engine', 'The run is not prepared.'),
    ev(36, 58, '15:13:10', 'prepared', 'runner', 'prepared on base commit 0e41aa7c95d2; the baseline verify passed'),
    ev(36, 58, '15:13:10', 'session_started', 'engine', 'readiness'),
    ev(36, 58, '15:13:44', 'session_ended', 'runner', 'ready after 2 turns'),
    ev(36, 58, '15:13:45', 'session_started', 'engine', 'start'),
    ...steps(36, 58, [
      ['15:14:02', 'Read', '.tool-versions'],
      ['15:14:20', 'Edit', '.tool-versions'],
      ['15:14:58', 'Edit', '.github/workflows/ci.yaml'],
      ['15:15:40', 'Bash', 'npm ci'],
      ['15:18:12', 'Bash', 'npm test'],
      ['15:19:30', '', 'Two suites fail on Node 24: the date tests read a locale string that changed.'],
      ['15:21:05', 'Edit', 'src/lib/dates.test.ts'],
      ['15:22:40', 'Bash', 'npm test'],
    ]),
    ev(36, 58, '15:24:02', 'session_ended', 'runner', 'done after 22 turns', { summary: 'Node is 24 in .tool-versions and in CI. The date tests now compare parts instead of a formatted string.', assumptions: ['The locale change in Node 24 is intended upstream.'], where_to_look: ['src/lib/dates.test.ts'] }),
    ev(36, 58, '15:24:03', 'verify_started', 'engine', 'The change has not been verified.'),
    ev(36, 58, '15:27:41', 'verify_ended', 'runner', 'failed', { result: 'failed', command: 'npm test', exit_code: 1, duration_ms: 218000, output: ' FAIL  src/pages/Board.test.tsx > Board > orders the columns\nTypeError: Cannot read properties of undefined (reading \'toSorted\')\n ❯ src/pages/Board.tsx:41:22\n\n Test Files  1 failed | 61 passed (62)\n      Tests  1 failed | 498 passed (499)' }),
    ev(36, 58, '15:27:42', 'session_started', 'engine', 'fix_verify'),
    ...steps(36, 58, [
      ['15:28:00', 'Read', 'src/pages/Board.tsx'],
      ['15:29:14', 'Edit', 'src/pages/Board.tsx'],
      ['15:30:02', 'Bash', 'npm test -- Board'],
    ]),
    ev(36, 58, '15:31:20', 'session_ended', 'runner', 'done after 9 turns', { summary: 'The board sorted a list that can be undefined before the first load.', assumptions: [], where_to_look: ['src/pages/Board.tsx:41'] }),
    ev(36, 58, '15:31:21', 'verify_started', 'engine', 'The change has not been verified.'),
    ev(36, 58, '15:35:02', 'verify_ended', 'runner', 'failed', { result: 'failed', command: 'npm test', exit_code: 1, duration_ms: 221000, output: ' FAIL  src/pages/Board.test.tsx > Board > orders the columns\nAssertionError: expected [ \'queued\', \'draft\', …(5) ] to deeply equal [ \'draft\', \'queued\', …(5) ]\n ❯ src/pages/Board.test.tsx:88:25\n\n Test Files  1 failed | 61 passed (62)\n      Tests  1 failed | 498 passed (499)' }),
    ev(36, 58, '15:35:03', 'session_started', 'engine', 'fix_verify'),
    ...steps(36, 58, [
      ['15:35:30', 'Read', 'src/pages/Board.test.tsx'],
      ['15:37:44', 'Edit', 'src/pages/Board.tsx'],
      ['15:38:20', 'Bash', 'npm test -- Board'],
    ]),
    ev(36, 58, '15:48:10', 'session_ended', 'runner', 'done after 33 turns', { summary: 'The column order no longer depends on the sort being stable.', assumptions: [], where_to_look: ['src/pages/Board.tsx:38-52'] }),
    ev(36, 58, '15:48:11', 'verify_started', 'engine', 'The change has not been verified.'),
    ev(36, 58, '15:51:58', 'verify_ended', 'runner', 'failed', { result: 'failed', command: 'npm test', exit_code: 1, duration_ms: 226000, output: ' FAIL  src/lib/dates.test.ts > formats a reset > in the local zone\nAssertionError: expected \'23.10\' to be \'23:10\'\n ❯ src/lib/dates.test.ts:52:31\n\n Test Files  1 failed | 61 passed (62)\n      Tests  1 failed | 498 passed (499)' }),
    ev(36, 58, '15:52:00', 'stopped', 'engine', 'verify_failed: The change did not pass the verify command.'),
    ev(36, 58, '15:52:00', 'run_ended', 'engine', 'the run ended: verify_failed'),
  ],
  41: [
    ev(41, null, '17:02:20', 'task_started', 'user', 'the task is started'),
    ev(41, 63, '17:02:20', 'run_started', 'engine', "the task's first run"),
    ev(41, 63, '17:02:21', 'prepare_started', 'engine', 'The run is not prepared.'),
    ev(41, 63, '17:04:01', 'prepared', 'runner', 'prepared on base commit 0e41aa7c95d2; the baseline verify passed'),
    ev(41, 63, '17:04:01', 'session_started', 'engine', 'readiness'),
    ...steps(41, 63, [
      ['17:04:20', 'Read', 'src/components/Layout.tsx'],
      ['17:04:38', 'Grep', 'changelog'],
      ['17:04:50', '', 'The header links to the changelog and the repository. The task names only the changelog for the footer.'],
    ]),
    ev(41, 63, '17:05:18', 'session_ended', 'runner', 'blocked after 6 turns'),
    ev(41, 63, '17:05:18', 'stopped', 'engine', 'question: Which pages should the footer link to?'),
  ],
  42: [
    ev(42, null, '19:21:10', 'task_started', 'user', 'the task is started'),
    ev(42, 66, '19:21:10', 'run_started', 'engine', "the task's first run"),
    ev(42, 66, '19:21:11', 'prepare_started', 'engine', 'The run is not prepared.'),
    ev(42, 66, '19:23:30', 'prepared', 'runner', 'prepared on base commit 934b98c1d2e3; the baseline verify passed'),
    ev(42, 66, '19:23:30', 'session_started', 'engine', 'readiness'),
    ev(42, 66, '19:24:02', 'session_ended', 'runner', 'ready after 2 turns'),
    ev(42, 66, '19:24:03', 'session_started', 'engine', 'start'),
    ...steps(42, 66, [
      ['19:24:20', 'Read', 'internal/api/api.go'],
      ['19:25:02', 'Read', 'cmd/cavu/serve.go'],
      ['19:26:40', '', 'The mux has only API routes. The files need one handler in front, with the page as the fallback.'],
      ['19:28:15', 'Write', 'web/embed.go'],
      ['19:31:50', 'Edit', 'internal/api/api.go'],
      ['19:35:22', 'Write', 'internal/api/files_test.go'],
      ['19:38:04', 'Bash', 'go test ./internal/api/'],
      ['19:39:52', 'Edit', 'internal/api/api.go'],
    ]),
  ],
};

// GET /api/settings: the named GitHub credentials a project may use
SETTINGS.forge_credentials = [{ name: 'acme-org', kind: 'token' }, { name: 'work-app', kind: 'app', app_id: 482913 }];

// GET /api/projects: docs-site's check failed at setup with a verify command, so its baseline did not pass
PROJECTS[2].baseline = { commit: PROJECTS[2].check.commit, passed: false, at: '2026-11-19T18:02:00Z', output: PROJECTS[2].check.output };

// The Settings page's: two named credentials, the server's defaults, and the credential each project names.
SETTINGS.forge_credentials = [{ name: 'acme-org', kind: 'token' }, { name: 'work-app', kind: 'app', app_id: 482913 }]
SETTINGS.defaults = {
  runtime: '', ca_bundle_path: '', endpoint_mode: '', endpoint_url: '', endpoint_auth_header: '',
  model_work: '', effort_work: '', model_review: '', effort_review: '', model_strong: '', effort_strong: '',
  task_timeout_minutes: '300', task_max_turns: '1000', task_budget_usd: '', daily_budget_usd: '', model_prices: '',
  verify_timeout_minutes: '10', commit_name: '', commit_email: '', concurrency: '3', listen_address: '127.0.0.1:7117',
  skills_path: '', instructions_path: '', forge_app_id: '', ci_timeout_minutes: '120',
}
for (const [name, credential] of [['cavu', ''], ['homelander', ''], ['docs-site', 'acme-org']]) {
  Object.assign(PROJECTS.find((p) => p.name === name)!, { forge_credential: credential })
}

// The setup page's: a fresh instance at each point of its setup, which the harness serves for setup=1|2|3|4|failed.
const UNSET = { endpoint_credential: false, forge_token: false, forge_app_key: false }
const ENDPOINT = { endpoint_mode: 'subscription', model_work: 'sonnet' }
const GITHUB = { settings: { ...ENDPOINT, commit_name: 'cavu', commit_email: 'cavu@example.com' }, secrets: { ...UNSET, endpoint_credential: true, forge_token: true } }
export const SETUP: Record<string, { settings: Settings['settings']; secrets: Settings['secrets']; projects: Project[] }> = {
  1: { settings: {}, secrets: UNSET, projects: [] },
  2: { settings: ENDPOINT, secrets: { ...UNSET, endpoint_credential: true }, projects: [] },
  3: { ...GITHUB, projects: [] },
  4: { ...GITHUB, projects: [{ ...PROJECTS[0], forge_credential: '' }] },
  failed: { ...GITHUB, projects: [{ ...PROJECTS[2], forge_credential: '' }] },
}

// A draft's page: a body of two short lines and two paragraphs that wrap, filed over MCP, and a dependency on a task that runs.
VIEWS[44] = base(44, {
  body: 'Add a section on the web app after the install steps.\nSay where it listens and how to sign in.\n\n' +
    "The section tells a reader who has just installed the binary what the web app is for. The Needs you page lists every task that waits for a person, with the way it waits and since when; the board shows every open task by its status; a task's page shows what happened in its runs, what it cost and the change it made. Keep it to one screen of the README, say that a phone gets the same pages with tabs along the bottom, link the API document for the routes the app calls, and leave screenshots out, since they would go stale with every change of the look.\n\n" +
    "Write it for a person who has never seen CAVU: say what a draft is and that it runs only when started, that a task stopped on a question waits for an answer on its page, and that a pull request is merged by a person and never by CAVU. Use the names the pages use, and check each sentence against the app as it is at the time.",
  requester: 'mcp',
  dependencies: [{ id: 42, status: 'running' }],
})
