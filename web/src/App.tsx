import { useCallback, useEffect, useRef, useState } from 'react'
import { Navigate, Outlet, Route, Routes, useLocation, useNavigate } from 'react-router'
import { api, onUnauthorized } from './api/client.ts'
import { useEvents, useStream } from './api/stream.ts'
import type { Project, Row } from './api/types.ts'
import Nav from './components/Nav.tsx'
import PhoneTabs from './components/PhoneTabs.tsx'
import { useNarrow } from './components/useNarrow.ts'
import { useVersion } from './components/useVersion.ts'
import Board from './pages/Board/Board.tsx'
import Login from './pages/Login/Login.tsx'
import More from './pages/More/More.tsx'
import NeedsYou from './pages/NeedsYou/NeedsYou.tsx'
import NewTask from './pages/NewTask/NewTask.tsx'
import Projects from './pages/Projects/Projects.tsx'
import Settings from './pages/Settings/Settings.tsx'
import Setup from './pages/Setup/Setup.tsx'
import Status from './pages/Status/Status.tsx'
import Task from './pages/Task/Task.tsx'
import './App.css'

function Frame() {
  const narrow = useNarrow()
  const version = useVersion()
  const [count, setCount] = useState(0)
  const [empty, setEmpty] = useState(false)

  // A new instance has no project: its first visit goes to setup, and nothing later does.
  useEffect(() => {
    api<{ projects: Project[] }>('GET', '/projects').then(
      (r) => setEmpty(r.projects.length === 0),
      () => {},
    )
  }, [])

  const read = useCallback(() => {
    api<{ tasks: Row[] }>('GET', '/tasks?needs_you=true')
      .then((r) => setCount(r.tasks.length))
      .catch(() => {}) // the next event or reconnect reads again
  }, [])

  const { live } = useEvents()
  useStream(read)

  if (empty) return <Navigate to="/setup" replace />

  return (
    <div className="frame">
      {!narrow && <Nav count={count} live={live} version={version} />}
      <main className="main">
        <Outlet />
      </main>
      {narrow && <PhoneTabs />}
    </div>
  )
}

export default function App() {
  const navigate = useNavigate()
  const location = useLocation()
  const here = useRef(location)
  useEffect(() => {
    here.current = location
  })
  // A 401 anywhere opens sign-in, which returns to the page the person was on.
  useEffect(() => {
    onUnauthorized(() => {
      const { pathname, search } = here.current
      if (pathname !== '/login') navigate('/login', { replace: true, state: { from: pathname + search } })
    })
  }, [navigate])

  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/setup" element={<Setup />} />
      <Route element={<Frame />}>
        <Route path="/" element={<NeedsYou />} />
        <Route path="/tasks/:id" element={<Task />} />
        <Route path="/board" element={<Board />} />
        <Route path="/projects" element={<Projects />} />
        <Route path="/projects/:name" element={<Projects />} />
        <Route path="/settings" element={<Settings />} />
        <Route path="/new" element={<NewTask />} />
        <Route path="/status" element={<Status />} />
        <Route path="/more" element={<More />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
