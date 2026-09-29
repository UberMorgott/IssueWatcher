// Settings keys of projects: platform:external_id (internal/config ProjectKey),
// e.g. github:owner/repo. The UI shows the project name instead of the key.

interface KeyedProject {
  key: string
  name: string
}

/** Display text of settings key: the known project's name, else the key without "github:". */
export function projectKeyLabel(key: string, repos: readonly KeyedProject[]): string {
  const r = repos.find((p) => p.key === key)
  if (r) return r.name
  return key.startsWith('github:') ? key.slice('github:'.length) : key
}

/** Select options (value = key, label = name) for the synced projects plus extra keys, by label. */
export function projectKeyOptions(repos: readonly KeyedProject[], extra: readonly string[] = []): { label: string; value: string }[] {
  const keys = new Set(repos.map((r) => r.key))
  for (const k of extra) if (k) keys.add(k)
  return [...keys].map((k) => ({ label: projectKeyLabel(k, repos), value: k })).sort((a, b) => a.label.localeCompare(b.label))
}
