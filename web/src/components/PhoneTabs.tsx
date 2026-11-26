import { Link, useLocation } from 'react-router'
import { AddIcon, InboxIcon, MoreIcon, ProjectsIcon, StatusIcon } from './icons.tsx'
import './PhoneTabs.css'

const tabs = [
  { to: '/', label: 'Needs you', icon: <InboxIcon /> },
  { to: '/status', label: 'Status', icon: <StatusIcon /> },
  { to: '/new', label: 'New task', icon: <AddIcon /> },
  { to: '/projects', label: 'Projects', icon: <ProjectsIcon /> },
  { to: '/more', label: 'More', icon: <MoreIcon /> },
]

export default function PhoneTabs() {
  const section = useLocation().pathname.split('/')[1].toLowerCase()
  const current = section === 'settings' ? '/more' : '/' + section
  return (
    <nav className="tabbar" aria-label="Main">
      {tabs.map(({ to, label, icon }) => (
        <Link
          key={to}
          to={to}
          aria-current={to === current ? 'page' : undefined}
          className={to === '/new' ? 'new-task-tab' : undefined}
        >
          {icon}
          <span>{label}</span>
        </Link>
      ))}
    </nav>
  )
}
