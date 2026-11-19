import { useCallback, useEffect, useRef, useState } from 'react'
import { Navigate, Outlet, Route, Routes } from 'react-router'
import { api } from './api/client.ts'
import { useEvents } from './api/stream.ts'
import type { Row } from './api/types.ts'
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

const readGap = 2000

function Frame() {
  const narrow = useNarrow()
  const version = useVersion()
  const [count, setCount] = useState(0)
  const lastRead = useRef(0)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  const read = useCallback(() => {
    lastRead.current = Date.now()
    api<{ tasks: Row[] }>('GET', '/tasks?needs_you=true')
      .then((r) => setCount(r.tasks.length))
      .catch(() => {}) // the next event or reconnect reads again
  }, [])

  // A burst of events leads to one read.
  const onEvent = useCallback(() => {
    if (timer.current !== undefined) return
    const wait = Math.max(0, lastRead.current + readGap - Date.now())
    timer.current = setTimeout(() => {
      timer.current = undefined
      read()
    }, wait)
  }, [read])

  useEffect(() => {
    read()
    return () => clearTimeout(timer.current)
  }, [read])

  const { live } = useEvents({ onEvent, onReconnect: read })

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
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route element={<Frame />}>
        <Route path="/" element={<NeedsYou />} />
        <Route path="/tasks/:id" element={<Task />} />
        <Route path="/board" element={<Board />} />
        <Route path="/projects" element={<Projects />} />
        <Route path="/projects/:name" element={<Projects />} />
        <Route path="/settings" element={<Settings />} />
        <Route path="/setup" element={<Setup />} />
        <Route path="/new" element={<NewTask />} />
        <Route path="/status" element={<Status />} />
        <Route path="/more" element={<More />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
