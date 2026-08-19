import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { bootstrapToken } from './api'
import './styles.css'

// Must run before the first request: it moves the token out of the URL and
// into storage.
bootstrapToken()

const root = document.getElementById('root')
if (!root) throw new Error('missing #root element')

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
