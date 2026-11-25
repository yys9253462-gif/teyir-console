// Keep each JSON input frame well below the backend's 16 KiB limit without
// splitting UTF-16 surrogate pairs (pasted emoji/CJK must remain intact).
export function terminalInputChunks(data: string): string[] {
  const points = Array.from(data)
  const chunks: string[] = []
  for (let offset = 0; offset < points.length; offset += 2048) chunks.push(points.slice(offset, offset + 2048).join(''))
  return chunks
}

export function parentDirectory(path: string): string {
  const trimmed = path.replace(/\/+$/, '')
  const slash = trimmed.lastIndexOf('/')
  return slash <= 0 ? '/' : trimmed.slice(0, slash)
}
