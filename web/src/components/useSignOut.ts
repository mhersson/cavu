import { useNavigate } from 'react-router'
import { api } from '../api/client.ts'
import { pushState, turnOff } from '../push.ts'

export function useSignOut(): () => Promise<void> {
  const navigate = useNavigate()
  return async () => {
    try {
      // turnOff waits for a worker that a browser whose registration failed never has, so only a browser with notifications on runs it.
      if (pushState() === 'on') await turnOff().catch(() => {})
      await api('POST', '/auth/logout')
    } finally {
      // The session is gone or cannot be reached: sign-in is the next step either way.
      navigate('/login')
    }
  }
}
