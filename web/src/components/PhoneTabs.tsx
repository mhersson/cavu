import { NavLink, useLocation } from 'react-router'
import './PhoneTabs.css'

const tabs: [string, string][] = [
  ['/', 'Needs you'],
  ['/new', 'New task'],
  ['/status', 'Status'],
  ['/more', 'More'],
]

// Pages reached through More keep its tab current.
const underMore = /^\/(settings|projects)(\/|$)/

export default function PhoneTabs() {
  const more = underMore.test(useLocation().pathname)
  return (
    <nav className="tabbar">
      {tabs.map(([to, label]) => (
        <NavLink key={to} to={to} end className={({ isActive }) => (isActive || (more && to === '/more') ? 'on' : '')}>
          {label}
        </NavLink>
      ))}
    </nav>
  )
}
