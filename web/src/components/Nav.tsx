import { NavLink } from 'react-router'
import './Nav.css'

type Props = { count: number; live: boolean; version: string }

const item = ({ isActive }: { isActive: boolean }) => 'nav-item' + (isActive ? ' on' : '')

export default function Nav({ count, live, version }: Props) {
  return (
    <nav className="nav">
      <span className="brand">CAVU</span>
      <NavLink to="/" end className={item}>
        Needs you
        {count > 0 && <span className="count">{count}</span>}
      </NavLink>
      <NavLink to="/board" className={item}>
        Board
      </NavLink>
      <NavLink to="/projects" className={item}>
        Projects
      </NavLink>
      <NavLink to="/settings" className={item}>
        Settings
      </NavLink>
      <div className="nav-foot">
        <span className="live" role="status" data-live={live}>
          {live ? 'Live' : 'Offline'}
        </span>
        <span>{version}</span>
      </div>
    </nav>
  )
}
