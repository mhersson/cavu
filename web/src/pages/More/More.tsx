import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import { api } from '../../api/client.ts'
import type { Project } from '../../api/types.ts'
import PageHead from '../../components/PageHead.tsx'
import { useSignOut } from '../../components/useSignOut.ts'
import { useVersion } from '../../components/useVersion.ts'
import './More.css'

export default function More() {
  const signOut = useSignOut()
  const version = useVersion()
  const [projects, setProjects] = useState<number | null>(null)
  useEffect(() => {
    api<{ projects: Project[] }>('GET', '/projects')
      .then((r) => setProjects(r.projects.length))
      .catch(() => {})
  }, [])

  return (
    <div className="page">
      <PageHead title="More" />
      <div className="more-list">
        <Link className="more-item" to="/projects">
          <span className="t1">Projects</span>
          {projects !== null && <span className="t2">{projects} projects</span>}
        </Link>
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
