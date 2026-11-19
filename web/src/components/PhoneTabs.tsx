import { NavLink } from 'react-router'
import './PhoneTabs.css'

const tabs: [string, string][] = [
  ['/', 'Needs you'],
  ['/new', 'New task'],
  ['/status', 'Status'],
  ['/more', 'More'],
]

export default function PhoneTabs() {
  return (
    <nav className="tabbar">
      {tabs.map(([to, label]) => (
        <NavLink key={to} to={to} end className={({ isActive }) => (isActive ? 'on' : '')}>
          {label}
        </NavLink>
      ))}
    </nav>
  )
}
