// Unified diff (git diff) → files → hunks → lines, and lazy syntax
// highlighting with highlight.js core + a few languages loaded on demand.
import type { HLJSApi, LanguageFn } from 'highlight.js'

export type LineKind = 'ctx' | 'add' | 'del' | 'meta'

export interface DiffLine {
  kind: LineKind
  text: string
  old?: number
  new?: number
}

export interface DiffHunk {
  header: string
  lines: DiffLine[]
}

export interface DiffFile {
  path: string
  oldPath: string
  status: 'A' | 'M' | 'D' | 'R'
  binary: boolean
  added: number
  deleted: number
  hunks: DiffHunk[]
  /** Lines to render (hunk headers included). */
  size: number
}

function unquote(p: string): string {
  if (p.startsWith('"') && p.endsWith('"')) p = p.slice(1, -1).replace(/\\(.)/g, '$1')
  return p.replace(/^[ab]\//, '')
}

const HUNK = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/

/** Parses `git diff` output; tolerant of truncated input (the server may cut large diffs). */
export function parseDiff(text: string): DiffFile[] {
  const files: DiffFile[] = []
  let f: DiffFile | null = null
  let h: DiffHunk | null = null
  let oldNo = 0
  let newNo = 0
  const lines = text.split('\n')
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i]
    if (l.startsWith('diff --git ')) {
      const m = /^diff --git (".*?"|\S+) (".*?"|\S+)$/.exec(l)
      const a = m ? unquote(m[1]) : ''
      const b = m ? unquote(m[2]) : l.slice(11)
      f = { path: b, oldPath: a, status: 'M', binary: false, added: 0, deleted: 0, hunks: [], size: 0 }
      files.push(f)
      h = null
      continue
    }
    if (!f) continue
    if (!h) {
      // File header lines before the first hunk.
      if (l.startsWith('new file mode')) f.status = 'A'
      else if (l.startsWith('deleted file mode')) f.status = 'D'
      else if (l.startsWith('rename from ')) {
        f.status = 'R'
        f.oldPath = l.slice(12)
      } else if (l.startsWith('rename to ')) f.path = l.slice(10)
      else if (l.startsWith('Binary files ') || l.startsWith('GIT binary patch')) f.binary = true
      else if (l.startsWith('--- ')) {
        const p = l.slice(4)
        if (p !== '/dev/null') f.oldPath = unquote(p)
      } else if (l.startsWith('+++ ')) {
        const p = l.slice(4)
        if (p !== '/dev/null') f.path = unquote(p)
        else f.path = f.oldPath
      }
    }
    const m = HUNK.exec(l)
    if (m) {
      h = { header: l, lines: [] }
      f.hunks.push(h)
      f.size++
      oldNo = Number(m[1])
      newNo = Number(m[2])
      continue
    }
    if (!h) continue
    const c = l[0]
    if (c === '+') {
      h.lines.push({ kind: 'add', text: l.slice(1), new: newNo++ })
      f.added++
    } else if (c === '-') {
      h.lines.push({ kind: 'del', text: l.slice(1), old: oldNo++ })
      f.deleted++
    } else if (c === ' ') {
      h.lines.push({ kind: 'ctx', text: l.slice(1), old: oldNo++, new: newNo++ })
    } else if (c === '\\') {
      h.lines.push({ kind: 'meta', text: l })
    } else if (l === '' && i === lines.length - 1) {
      continue // trailing newline of the whole diff
    } else {
      h.lines.push({ kind: 'ctx', text: l, old: oldNo++, new: newNo++ })
    }
    f.size++
  }
  return files
}

// --- syntax highlighting -----------------------------------------------------

type Loader = () => Promise<{ default: LanguageFn }>

const LOADERS: Record<string, Loader> = {
  go: () => import('highlight.js/lib/languages/go'),
  typescript: () => import('highlight.js/lib/languages/typescript'),
  javascript: () => import('highlight.js/lib/languages/javascript'),
  xml: () => import('highlight.js/lib/languages/xml'),
  css: () => import('highlight.js/lib/languages/css'),
  scss: () => import('highlight.js/lib/languages/scss'),
  json: () => import('highlight.js/lib/languages/json'),
  python: () => import('highlight.js/lib/languages/python'),
  csharp: () => import('highlight.js/lib/languages/csharp'),
  cpp: () => import('highlight.js/lib/languages/cpp'),
  rust: () => import('highlight.js/lib/languages/rust'),
  java: () => import('highlight.js/lib/languages/java'),
  lua: () => import('highlight.js/lib/languages/lua'),
  sql: () => import('highlight.js/lib/languages/sql'),
  yaml: () => import('highlight.js/lib/languages/yaml'),
  markdown: () => import('highlight.js/lib/languages/markdown'),
  powershell: () => import('highlight.js/lib/languages/powershell'),
  bash: () => import('highlight.js/lib/languages/bash'),
  ini: () => import('highlight.js/lib/languages/ini'),
}

const EXT: Record<string, string> = {
  go: 'go',
  ts: 'typescript',
  tsx: 'typescript',
  mts: 'typescript',
  cts: 'typescript',
  js: 'javascript',
  jsx: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  vue: 'xml',
  html: 'xml',
  htm: 'xml',
  xml: 'xml',
  svg: 'xml',
  csproj: 'xml',
  props: 'xml',
  css: 'css',
  scss: 'scss',
  json: 'json',
  py: 'python',
  cs: 'csharp',
  c: 'cpp',
  h: 'cpp',
  cc: 'cpp',
  cpp: 'cpp',
  hpp: 'cpp',
  rs: 'rust',
  java: 'java',
  lua: 'lua',
  sql: 'sql',
  yml: 'yaml',
  yaml: 'yaml',
  md: 'markdown',
  ps1: 'powershell',
  psm1: 'powershell',
  sh: 'bash',
  bash: 'bash',
  toml: 'ini',
  ini: 'ini',
}

/** highlight.js language for a file path; "" = plain text. */
export function languageOf(path: string): string {
  const name = path.slice(path.lastIndexOf('/') + 1).toLowerCase()
  if (name === 'dockerfile' || name === 'makefile') return ''
  const dot = name.lastIndexOf('.')
  return dot >= 0 ? (EXT[name.slice(dot + 1)] ?? '') : ''
}

let core: Promise<HLJSApi> | null = null
const loaded = new Map<string, Promise<boolean>>()

/** Loads highlight.js core and one language; resolves to the API, or null when unsupported. */
export async function highlighter(lang: string): Promise<HLJSApi | null> {
  const load = LOADERS[lang]
  if (!load) return null
  core ??= import('highlight.js/lib/core').then((m) => m.default)
  const hljs = await core
  let p = loaded.get(lang)
  if (!p) {
    p = load()
      .then((m) => {
        hljs.registerLanguage(lang, m.default)
        return true
      })
      .catch(() => false)
    loaded.set(lang, p)
  }
  return (await p) ? hljs : null
}

/**
 * Highlights diff lines one by one (each line on its own: multi-line constructs
 * such as block comments lose their colour, the price of per-line rendering).
 * Returns escaped HTML per line.
 */
export function highlightLines(hljs: HLJSApi, lang: string, lines: string[]): string[] {
  return lines.map((l) => (l ? hljs.highlight(l, { language: lang, ignoreIllegals: true }).value : ''))
}
