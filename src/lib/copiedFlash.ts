import { useCallback, useEffect, useRef, useState } from 'react'

// useCopiedFlash —— 「已复制」绿色小提示。
//
// 为什么需要它（2026-10-30）：原来的写法是
//   setCopied(label)
//   window.setTimeout(() => setCopied(''), 2000)
// 没有任何 ref 持有 timer，也没有 cleanup。弹窗/页面 2 秒内被关掉时，
// 回调仍然对已卸载组件 setState —— React 18 严格模式下会告警，高频
// 打开/关闭时控制台被刷屏，看起来像内存泄漏。
//
// 这个 hook 做的事只有一件：把 setState 包在 ref 持有的 timer 里，
// 卸载时清掉 timer。业务代码照旧写 `flash(label)`。
export function useCopiedFlash(): [string, (label: string) => void] {
  const [copied, setCopied] = useState('')
  const timer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(timer.current), [])
  const flash = useCallback((label: string) => {
    window.clearTimeout(timer.current)
    setCopied(label)
    timer.current = window.setTimeout(() => setCopied(''), 2000)
  }, [])
  return [copied, flash]
}