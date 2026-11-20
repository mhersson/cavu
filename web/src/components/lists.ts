import { api } from '../api/client.ts'
import type { Row, Status } from '../api/types.ts'

export const open: Status[] = ['draft', 'queued', 'running', 'blocked', 'in_review']
export const finishedLimit = 30

// The open tasks, then with finished the newest done and the newest cancelled, of one project or of all.
export function readLists(finished: boolean, project = ''): Promise<Row[]> {
  const scope = project ? '&project=' + encodeURIComponent(project) : ''
  const tail = `&order=newest&limit=${finishedLimit}` + scope
  const paths = ['/tasks?status=' + open.join(',') + scope]
  if (finished) paths.push('/tasks?status=done' + tail, '/tasks?status=cancelled' + tail)
  return Promise.all(paths.map((p) => api<{ tasks: Row[] }>('GET', p))).then((lists) => lists.flatMap((l) => l.tasks))
}

// A finished list at its limit is the newest of more, so its count is not a total.
export function count(status: Status, n: number): number | string {
  return !open.includes(status) && n === finishedLimit ? `newest ${finishedLimit}` : n
}
