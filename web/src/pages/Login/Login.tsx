import { useState } from 'react'
import type { FormEvent } from 'react'
import { useLocation, useNavigate } from 'react-router'
import { api } from '../../api/client.ts'
import { useVersion } from '../../components/useVersion.ts'
import './Login.css'

export default function Login() {
  const navigate = useNavigate()
  const from = (useLocation().state as { from?: string } | null)?.from ?? '/'
  const version = useVersion()
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')

  async function submit(e: FormEvent) {
    e.preventDefault()
    try {
      await api('POST', '/auth/login', { name, password })
      navigate(from, { replace: true })
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    }
  }

  return (
    <>
      <div className="brand-bar">
        <span className="brand">CAVU</span>
        <span>{version}</span>
      </div>
      <form className="login" onSubmit={(e) => void submit(e)}>
        <h1>Sign in</h1>
        <div className="field">
          <label htmlFor="login-name">Name</label>
          <input id="login-name" name="name" autoComplete="username" autoFocus value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="login-password">Password</label>
          <input id="login-password" name="password" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </div>
        {error && <p role="alert" className="login-error">{error}</p>}
        <button className="btn primary login-submit">Sign in</button>
        <p className="sub">
          CAVU has one user, made on the server with <code>cavu user set</code>.
        </p>
      </form>
    </>
  )
}
