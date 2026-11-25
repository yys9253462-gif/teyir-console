import React from 'react'
import { AlertTriangle, RefreshCw } from 'lucide-react'

type ErrorBoundaryProps = {
  children: React.ReactNode
  /** 局部作用域名称，方便定位异常来源 */
  scope?: string
  /** 外部变化键（如切换导航页时自动重置异常状态） */
  resetKey?: string
}

type ErrorBoundaryState = {
  hasError: boolean
  message: string
}

// 渲染异常边界：防止单个子页面或分包加载异常导致整个控制台或买家交付页白屏。
export class ErrorBoundary extends React.Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { hasError: false, message: '' }

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    const message = error instanceof Error ? error.message : '页面渲染发生未知错误'
    return { hasError: true, message }
  }

  componentDidCatch(error: unknown, info: React.ErrorInfo): void {
    console.error(`[界面异常边界${this.props.scope ? `·${this.props.scope}` : ''}]`, error, info.componentStack)
  }

  componentDidUpdate(prevProps: ErrorBoundaryProps): void {
    if (this.state.hasError && prevProps.resetKey !== this.props.resetKey) {
      this.setState({ hasError: false, message: '' })
    }
  }

  private handleRetry = (): void => {
    this.setState({ hasError: false, message: '' })
  }

  render(): React.ReactNode {
    if (!this.state.hasError) return this.props.children
    return (
      <div className="panel error-boundary-card" role="alert">
        <div className="error-boundary-head">
          <AlertTriangle size={20} />
          <strong>{this.props.scope ? `${this.props.scope}渲染异常` : '页面渲染异常'}</strong>
        </div>
        <p className="form-note">
          当前视图遇到未预期错误，已拦截避免整页白屏。你可以先点击重试，若刚完成版本更新请刷新浏览器重新加载静态资源。
        </p>
        {this.state.message && <code className="error-boundary-detail">{this.state.message}</code>}
        <div className="error-boundary-actions">
          <button type="button" className="button button-primary" onClick={this.handleRetry}>
            <RefreshCw size={14} />重新渲染
          </button>
          <button type="button" className="button button-quiet" onClick={() => window.location.reload()}>
            刷新页面
          </button>
        </div>
      </div>
    )
  }
}

// 分包加载占位组件：替代空 fallback，避免弱网或按需切页时短暂白屏无反馈。
export function ViewLoadingFallback({ label = '正在加载界面…' }: { label?: string }) {
  return (
    <div className="panel view-loading-card" role="status" aria-live="polite">
      <span className="view-loading-dot" />
      <span>{label}</span>
    </div>
  )
}
