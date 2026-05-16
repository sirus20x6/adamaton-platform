// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { Routes, Route, Link, useLocation } from 'react-router-dom'
import Workflows from './pages/Workflows'
import Builder from './pages/Builder'
import Runs from './pages/Runs'
import Delegator from './pages/Delegator'
import Memory from './pages/Memory'
import ErrorBoundary from './components/ErrorBoundary'

const navItems = [
  { path: '/', label: 'Workflows' },
  { path: '/runs', label: 'Runs' },
  { path: '/delegator', label: 'Delegator' },
  { path: '/memory', label: 'Memory' },
]

export default function App() {
  const location = useLocation()
  const isBuilder = location.pathname.startsWith('/builder')

  if (isBuilder) {
    return (
      // Keying ErrorBoundary on pathname forces a fresh boundary instance per
      // route — once an error is caught, navigating away unmounts the failed
      // tree and the new route gets a clean boundary.
      <ErrorBoundary key={location.pathname}>
        <Routes>
          <Route path="/builder" element={<Builder />} />
          <Route path="/builder/:id" element={<Builder />} />
        </Routes>
      </ErrorBoundary>
    )
  }

  return (
    <div className="min-h-screen bg-zinc-950 text-zinc-100">
      <nav className="border-b border-zinc-800 bg-zinc-950/80 backdrop-blur-sm sticky top-0 z-50">
        <div className="max-w-6xl mx-auto px-6 h-14 flex items-center gap-8">
          <Link to="/" className="text-lg font-semibold tracking-tight text-white">
            GoGents
          </Link>
          <div className="flex gap-1">
            {navItems.map(item => (
              <Link
                key={item.path}
                to={item.path}
                className={`px-3 py-1.5 rounded-md text-sm transition-colors ${
                  location.pathname === item.path
                    ? 'bg-zinc-800 text-white'
                    : 'text-zinc-400 hover:text-zinc-200 hover:bg-zinc-800/50'
                }`}
              >
                {item.label}
              </Link>
            ))}
          </div>
        </div>
      </nav>
      <main className="max-w-6xl mx-auto px-6 py-8">
        <ErrorBoundary key={location.pathname}>
          <Routes>
            <Route path="/" element={<Workflows />} />
            <Route path="/runs" element={<Runs />} />
            <Route path="/delegator" element={<Delegator />} />
            <Route path="/memory" element={<Memory />} />
          </Routes>
        </ErrorBoundary>
      </main>
    </div>
  )
}