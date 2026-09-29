// Provider URLs (issue, comment, project pages) come from third parties
// (CFWidget, MCP servers, APIs). Bound to href, a javascript: URL would run in
// the dashboard origin, which can send replies and start agent jobs: only
// http(s) URLs are linked.
export function safeUrl(u: string | null | undefined): string | undefined {
  if (!u) return undefined
  try {
    const p = new URL(u)
    return p.protocol === 'https:' || p.protocol === 'http:' ? p.href : undefined
  } catch {
    return undefined
  }
}
