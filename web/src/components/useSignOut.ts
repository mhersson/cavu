import { useNavigate } from 'react-router'
import { api } from '../api/client.ts'

export function useSignOut(): () => Promise<void> {
  const navigate = useNavigate()
  return async () => {
    try {
      await api('POST', '/auth/logout')
    } finally {
      // The session is gone or cannot be reached: sign-in is the next step either way.
      navigate('/login')
    }
  }
}
