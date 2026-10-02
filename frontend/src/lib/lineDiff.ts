// Line diff of two texts (LCS), for «what changes» previews (mod page edit).

export interface TextDiffLine {
  kind: 'ctx' | 'add' | 'del'
  text: string
}

/** Above this many cell comparisons the diff shows the whole text replaced (keeps the UI responsive). */
const MAX_CELLS = 4_000_000

/** Lines of b against a: unchanged (ctx), removed (del), added (add), in order. */
export function lineDiff(a: string, b: string): TextDiffLine[] {
  const x = a.split('\n')
  const y = b.split('\n')
  if (a === b) return x.map((text) => ({ kind: 'ctx', text }))
  // Common head and tail first: an edit usually touches a few lines.
  let head = 0
  while (head < x.length && head < y.length && x[head] === y[head]) head++
  let tail = 0
  while (tail < x.length - head && tail < y.length - head && x[x.length - 1 - tail] === y[y.length - 1 - tail]) tail++
  const xs = x.slice(head, x.length - tail)
  const ys = y.slice(head, y.length - tail)
  const out: TextDiffLine[] = x.slice(0, head).map((text) => ({ kind: 'ctx', text }))
  if (xs.length * ys.length > MAX_CELLS) {
    out.push(...xs.map((text) => ({ kind: 'del' as const, text })), ...ys.map((text) => ({ kind: 'add' as const, text })))
  } else {
    // lcs[i][j] = LCS length of xs[i:] and ys[j:].
    const n = xs.length
    const m = ys.length
    const lcs: Uint32Array[] = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1))
    for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) lcs[i][j] = xs[i] === ys[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1])
    let i = 0
    let j = 0
    while (i < n || j < m) {
      if (i < n && j < m && xs[i] === ys[j]) {
        out.push({ kind: 'ctx', text: xs[i] })
        i++
        j++
      } else if (i < n && (j >= m || lcs[i + 1][j] >= lcs[i][j + 1])) {
        out.push({ kind: 'del', text: xs[i++] }) // removed lines before their replacement
      } else {
        out.push({ kind: 'add', text: ys[j++] })
      }
    }
  }
  out.push(...x.slice(x.length - tail).map((text) => ({ kind: 'ctx' as const, text })))
  return out
}

/** Folds long unchanged runs to `keep` lines of context around each change (null = a fold marker). */
export function foldContext(lines: TextDiffLine[], keep = 2): (TextDiffLine | null)[] {
  const near = lines.map(() => false)
  lines.forEach((l, i) => {
    if (l.kind === 'ctx') return
    for (let k = Math.max(0, i - keep); k <= Math.min(lines.length - 1, i + keep); k++) near[k] = true
  })
  const out: (TextDiffLine | null)[] = []
  lines.forEach((l, i) => {
    if (near[i]) out.push(l)
    else if (out[out.length - 1] !== null) out.push(null)
  })
  return out
}
