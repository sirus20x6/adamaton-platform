// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import React from 'react'

interface State {
  error?: Error
  componentStack?: string | null
}

interface Props {
  children: React.ReactNode
}

export class ErrorBoundary extends React.Component<Props, State> {
  constructor(props: Props) {
    super(props)
    this.state = {}
  }

  static getDerivedStateFromError(error: Error): State {
    return { error }
  }

  componentDidCatch(err: Error, info: React.ErrorInfo) {
    // Log to console for now; a future enhancement could ship this to a
    // server-side error endpoint.
    console.error('UI error:', err, info)
    this.setState({ componentStack: info.componentStack ?? null })
  }

  render() {
    if (this.state.error) {
      const isDev = import.meta.env.DEV
      return (
        <div className="p-8 max-w-2xl mx-auto">
          <h1 className="text-xl font-semibold text-red-400 mb-2">Something broke.</h1>
          <pre className="text-xs text-zinc-400 whitespace-pre-wrap mb-4">{this.state.error.message}</pre>
          {isDev && this.state.componentStack && (
            <details className="mb-4">
              <summary className="text-[10px] text-zinc-500 cursor-pointer hover:text-zinc-300">
                Component stack (dev only)
              </summary>
              <pre className="text-[10px] text-zinc-500 whitespace-pre-wrap mt-2 bg-zinc-900 p-2 rounded">
                {this.state.componentStack}
              </pre>
            </details>
          )}
          <div className="flex gap-2">
            <button
              onClick={() => window.location.reload()}
              className="px-3 py-1.5 bg-indigo-600 hover:bg-indigo-500 text-white text-sm rounded-md transition-colors"
            >
              Reload page
            </button>
            <a
              href="/"
              className="px-3 py-1.5 bg-zinc-800 hover:bg-zinc-700 text-zinc-300 text-sm rounded-md transition-colors inline-block"
            >
              Go home
            </a>
          </div>
        </div>
      )
    }
    return this.props.children
  }
}

export default ErrorBoundary