import type { ReactNode } from 'react'

export function HostOpsError({ error }: { error: string }) {
  return error ? <div className="host-ops-error" role="alert">{error}</div> : null
}

export function HostOpsTable({ columns, rows, empty }: { columns: string[]; rows: ReactNode[][]; empty: string }) {
  return <div className="host-ops-table-wrap"><table className="host-ops-table">
    <thead><tr>{columns.map((column) => <th key={column} scope="col">{column}</th>)}</tr></thead>
    <tbody>{rows.length ? rows.map((row, index) => <tr key={index}>{row.map((cell, cellIndex) => <td key={cellIndex}>{cell}</td>)}</tr>)
      : <tr><td colSpan={columns.length}>{empty}</td></tr>}</tbody>
  </table></div>
}

export function hostOpsError(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
