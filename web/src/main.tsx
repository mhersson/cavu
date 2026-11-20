import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import './theme/tokens.css'
import './theme/base.css'
import './theme/theme.ts'
import App from './App.tsx'
import { register } from './push.ts'

register()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
)
