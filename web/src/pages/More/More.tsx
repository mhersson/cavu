import { Link } from 'react-router'
import PageHead from '../../components/PageHead.tsx'
import { useSignOut } from '../../components/useSignOut.ts'
import { useVersion } from '../../components/useVersion.ts'
import './More.css'

export default function More() {
  const signOut = useSignOut()
  const version = useVersion()

  return (
    <div className="page">
      <PageHead title="More" />
      <div className="more-list">
        <Link className="more-item" to="/settings">
          <span className="t1">Settings</span>
          <span className="t2">The model endpoint, models, limits, GitHub</span>
        </Link>
        <button className="more-item" onClick={() => void signOut()}>
          <span className="t1">Sign out</span>
        </button>
      </div>
      <p className="more-version">CAVU {version}</p>
    </div>
  )
}
