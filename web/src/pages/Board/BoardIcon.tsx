import { Icon } from '../../components/icons.tsx'

const ring = <circle cx="8" cy="8" r="6" />
const shapes = {
  running: <>{ring}<path d="m6.5 5 4.5 3-4.5 3z" fill="currentColor" stroke="none" /></>,
  queued: <>{ring}<path d="M8 4.5V8l2.5 1.5" /></>,
  blocked: <path d="m5.5 2-3.5 3.5v5l3.5 3.5h5l3.5-3.5v-5L10.5 2zM5.5 8h5" />,
  in_review: <><circle cx="4" cy="3.5" r="1.5" /><circle cx="4" cy="12.5" r="1.5" /><circle cx="12" cy="12.5" r="1.5" /><path d="M4 5v6m8 0V8c0-3-1.5-4-4-4m1.5-2L7.5 4l2 2" /></>,
  draft: <path d="M4 2h5l3 3v9H4zM9 2v4h3m-6 3h4m-4 2.5h3" />,
  done: <>{ring}<path d="m5 8 2 2 4-4" /></>,
  cancelled: <>{ring}<path d="m4 12 8-8" /></>,
  search: <><circle cx="7" cy="7" r="4.5" /><path d="m10.5 10.5 3.5 3.5" /></>,
  project: <path d="M3.5 4c0-1 .7-2 2-2H13v12H5.5a1.5 1.5 0 0 1 0-3H13M3.5 4v8.5" />,
  chevron: <path d="m4 6 4 4 4-4" />,
  person: <><circle cx="8" cy="5" r="2" /><path d="M3.5 14v-1.5a4.5 4.5 0 0 1 9 0V14" /></>,
}

export default function BoardIcon({ name }: { name: keyof typeof shapes }) {
  return <Icon>{shapes[name]}</Icon>
}
