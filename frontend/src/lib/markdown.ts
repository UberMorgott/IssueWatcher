// A safe Markdown subset for GitHub issue/comment bodies: output is plain
// tokens rendered as Vue nodes (text is never parsed as HTML), so a body can't
// inject markup. Raw HTML in a body stays visible text.
// shortcut: fences, inline code, bold, italic, links, headings only; lists,
// tables, quotes stay plain text (pre-wrap keeps them readable). Upgrade to a
// full parser + sanitizer if richer bodies are needed.
export type Inline = { t: 'text' | 'code' | 'b' | 'i'; v: string } | { t: 'a'; v: string; href: string }
export type Block = { t: 'pre'; v: string } | { t: 'text'; v: Inline[] }

const FENCE = /^ {0,3}(`{3,}|~{3,})/
const INLINE =
  /`([^`\n]+)`|\*\*([^*\n]+?)\*\*|__([^_\n]+?)__|\[([^\]\n]+)\]\(([^)\s]+)\)|(?<![\w*])\*(?![\s*])([^*\n]+?)(?<!\s)\*(?![\w*])|(?<![\w_])_(?![\s_])([^_\n]+?)(?<!\s)_(?![\w_])/g

export function inline(s: string): Inline[] {
  const out: Inline[] = []
  let last = 0
  const text = (v: string) => {
    if (!v) return
    const prev = out.at(-1)
    if (prev?.t === 'text') prev.v += v
    else out.push({ t: 'text', v })
  }
  for (const m of s.matchAll(INLINE)) {
    text(s.slice(last, m.index))
    last = m.index + m[0].length
    if (m[1] !== undefined) out.push({ t: 'code', v: m[1] })
    else if (m[2] ?? m[3]) out.push({ t: 'b', v: (m[2] ?? m[3]) as string })
    else if (m[4] !== undefined) out.push({ t: 'a', v: m[4], href: m[5] }) // href unchecked: the renderer links http(s) only
    else out.push({ t: 'i', v: (m[6] ?? m[7]) as string })
  }
  text(s.slice(last))
  return out
}

/** ATX heading line → bold text without the hashes. */
function line(l: string): Inline[] {
  const h = /^ {0,3}#{1,6}\s+(.*?)(?:\s+#+)?\s*$/.exec(l)
  return h ? [{ t: 'b', v: h[1] }] : inline(l)
}

export function markdown(src: string): Block[] {
  const out: Block[] = []
  let text: string[] = []
  let code: string[] | null = null
  let fence = ''
  const flush = () => {
    while (text.length && !text[0].trim()) text.shift() // blocks carry their own spacing
    while (text.length && !text.at(-1)?.trim()) text.pop()
    if (!text.length) return
    const v: Inline[] = []
    text.forEach((l, i) => v.push(...line(l), ...(i < text.length - 1 ? [{ t: 'text' as const, v: '\n' }] : [])))
    out.push({ t: 'text', v })
    text = []
  }
  for (const l of src.replace(/\r\n?/g, '\n').split('\n')) {
    const f = FENCE.exec(l)
    if (code) {
      if (f && f[1][0] === fence[0] && f[1].length >= fence.length && !l.slice(f[0].length).trim()) {
        out.push({ t: 'pre', v: code.join('\n') })
        code = null
      } else code.push(l)
    } else if (f) {
      flush()
      code = []
      fence = f[1]
    } else text.push(l)
  }
  if (code) out.push({ t: 'pre', v: code.join('\n') }) // unclosed fence runs to the end, as on GitHub
  flush()
  return out
}
