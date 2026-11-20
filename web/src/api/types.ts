// The JSON shapes of docs/api.md, field for field.

export type Status =
  | 'draft'
  | 'queued'
  | 'running'
  | 'blocked'
  | 'in_review'
  | 'done'
  | 'cancelled'

export type StopReason =
  | 'question'
  | 'environment'
  | 'verify_failed'
  | 'budget'
  | 'timeout'
  | 'no_change'
  | 'agent_error'
  | 'usage_limit'
  | 'endpoint'
  | 'sandbox'
  | 'cancelled'
  | 'dependency'
  | 'push_rejected'
  | 'ci_failed'
  | 'ci_timeout'
  | 'no_checks'
  | 'conflict'
  | 'taken_over'
  | 'changes_requested'

export type Stop = {
  reason: StopReason
  explanation: string
  action: 'answer' | 'retry' | 'none'
  resets_at: string | null
}

export type Question = { question: string; suggested_answer: string }

export type Finding = {
  severity: 'critical' | 'important' | 'minor'
  where: string
  what: string
  why: string
}

export type Review = { rounds: number; failed: string; open: Finding[]; minor: Finding[] }

export type PullRequest = {
  number: number
  url: string
  branch: string
  head: string
  state: 'open' | 'merged' | 'closed'
  checks: '' | 'none' | 'pending' | 'passed' | 'failed'
  failed_checks: string[]
  ready: boolean
}

export type Change = {
  head_commit: string
  files_changed: number
  diff_stat: string
  flagged: { workflows: string[]; instructions: string[]; large: string[] }
}

export type Usage = {
  source: 'proxy' | 'agent'
  model: string
  input: number
  output: number
  cache_read: number
  cache_write: number
  cost_estimate_usd: number
}

export type Budget = {
  turns: number
  max_turns: number
  minutes: number
  max_minutes: number
  usd?: number
  max_usd?: number
  day_usd?: number
  max_day_usd?: number
}

export type Task = {
  id: number
  project: string
  title: string
  body: string
  requester: 'cli' | 'api' | 'mcp'
  created_at: string
  status: Status
  needs_you: boolean
  waiting: string
  waiting_until: string | null
  dependencies: { id: number; status: Status }[]
  stop: Stop | null
  questions: Question[]
  review: Review | null
  pull_request: PullRequest | null
  change: Change | null
  turns: number
  duration_ms: number
  cost_estimate_usd: number
  cost_missing_sessions: number
  usage: Usage[]
  budget: Budget
}

// An item of GET /api/tasks: some of the task's fields, and two derived ones.
export type Row = {
  id: number
  project: string
  title: string
  status: Status
  needs_you: boolean
  stop_reason: StopReason | ''
  waiting: string
  pull_request_ready: boolean
  created_at: string
}

export type TaskEvent = {
  id: number
  task_id: number
  run_id: number | null
  at: string
  type: string
  actor: 'user' | 'engine' | 'runner' | 'agent'
  payload: unknown
  text: string
}

export type Project = {
  id: number
  name: string
  repo_url: string
  base_branch: string
  setup_command: string
  verify_command: string
  verify_waived: boolean
  tool_overrides: Record<string, string>
  egress_hosts: string[]
  env: Record<string, string>
  secret_vars: string[]
  switches: Record<string, boolean>
  caps: Record<string, string>
  no_ci: boolean
  ignored_checks: string[]
  review_bots: string[]
  runnable: boolean
  baseline: { commit: string; passed: boolean; at: string; output: string } | null
  check: {
    requested_at: string | null
    tools: string[]
    ignored_tools: string[]
    failed_step: string
    verify_suggestion: string
    fault: string
    commit: string
    output: string
  }
}

export type Settings = {
  settings: Record<string, string | Record<string, unknown>>
  secrets: Record<string, boolean>
}
