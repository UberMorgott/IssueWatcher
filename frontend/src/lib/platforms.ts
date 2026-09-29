import type { Capabilities, ItemKind } from '../api/types'

/** Platforms in display order (GitHub first). */
export const PLATFORMS = ['github', 'nexus', 'curseforge', 'steam'] as const
export type PlatformId = (typeof PLATFORMS)[number]

export const PLATFORM_NAMES: Record<string, string> = {
  github: 'GitHub',
  nexus: 'Nexus Mods',
  curseforge: 'CurseForge',
  steam: 'Steam Workshop',
}

export const platformName = (id: string) => PLATFORM_NAMES[id] ?? id

/** Mod platforms: their projects are mod pages, linked to a GitHub code project. */
export const isModPlatform = (id: string) => !!id && id !== 'github'

export const ITEM_KINDS: ItemKind[] = ['issue', 'comment', 'bug']

/** What a platform's items allow when its capabilities are not loaded yet. */
export function fallbackCaps(platform: string): Capabilities {
  const gh = platform === 'github' || !platform
  return {
    listProjects: true,
    syncItems: true,
    listComments: true,
    reply: platform !== 'steam',
    setLabels: gh,
    setStatus: false,
    createPR: gh,
    auth: '',
    kinds: gh ? ['issue'] : platform === 'nexus' ? ['comment', 'bug'] : ['comment'],
    replyThreaded: platform !== 'steam',
  }
}

/** Item reference for crumbs and toasts: owner/repo#12 on GitHub, the mod page name elsewhere. */
export const itemRef = (it: { repo: string; number: number; platform?: string }) =>
  isModPlatform(it.platform ?? '') ? it.repo : `${it.repo}#${it.number}`
