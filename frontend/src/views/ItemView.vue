<script setup lang="ts">
import { safeUrl } from '../lib/safeUrl'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import Button from 'primevue/button'
import Textarea from 'primevue/textarea'
import Skeleton from 'primevue/skeleton'
import Message from 'primevue/message'
import { useToast } from 'primevue/usetoast'
import EmptyState from '../components/EmptyState.vue'
import LabelTag from '../components/LabelTag.vue'
import PlatformIcon from '../components/PlatformIcon.vue'
import { api } from '../api/client'
import type { IssueDetail, Job, JobFlow } from '../api/types'
import SplitButton from 'primevue/splitbutton'
import JobBadge from '../components/JobBadge.vue'
import JobProgress from '../components/JobProgress.vue'
import JobLog from '../components/JobLog.vue'
import DirectResult from '../components/DirectResult.vue'
import FolderDialog from '../components/FolderDialog.vue'
import { canPush, FLOW_ICON, isActive, isDirect, JOB_FLOWS, jobOutcome, useDispatchToast, usePush } from '../lib/jobs'
import { useJobEvents, useJobsStore } from '../stores/jobs'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, num, relTime } from '../lib/format'
import { useCrumbs } from '../lib/crumbs'
import { useChunks } from '../lib/chunks'
import { commentsCache, itemCache, itemJobsCache } from '../lib/cache'
import { isModPlatform, itemRef, platformName, replyLength, replyLimit, replyTooLong } from '../lib/platforms'
import { currentItemKind } from '../lib/currentItem'
import { typing } from '../lib/shortcuts'

const props = defineProps<{ id: string }>()
const app = useAppStore()
const { t } = useI18n()
const toast = useToast()

const item = ref<IssueDetail | null>(null)
const state = ref<'loading' | 'ok' | 'missing' | 'unavailable' | 'error'>('loading')
const errorText = ref('')
const reply = ref('')
const sending = ref(false)
const replyError = ref('')

// Generation guard: only the newest load may write the page (a late answer for
// the previous item, or an older quiet refresh, is dropped).
let loadGen = 0
/** «Непрочитано» clicked on this item: refreshes leave it unread until the reader leaves. */
let keptUnread = false
async function load(quiet = false) {
  if (!quiet) state.value = 'loading'
  const id = props.id
  const g = ++loadGen
  const r = await api.issue(id)
  if (g !== loadGen || id !== props.id) return
  if (!r.ok) {
    state.value = r.status === 404 ? (r.error === 'not found' ? 'missing' : 'unavailable') : r.status === 400 ? 'missing' : 'error'
    errorText.value = r.error
    return
  }
  item.value = r.data
  state.value = 'ok'
  // Steam has no threads: a reply is a new comment addressed to the author (never to oneself).
  if (!reply.value && !app.caps(r.data.platform || 'github').replyThreaded && r.data.author && !r.data.mine) reply.value = '@' + r.data.author + ' '
  // Opening marks the item read, unless the reader just marked it unread here.
  if (r.data.unread && !keptUnread) {
    const m = await api.markRead(r.data.id)
    if (m.ok) {
      if (g === loadGen && id === props.id) item.value = { ...r.data, unread: false }
      void app.loadRepos()
    }
  }
}

// Comments arrive in chunks while the reader scrolls towards the end (sentinel
// ~2 screens ahead); skeleton posts mark the loading tail.
const comments = useChunks((cursor) => api.comments(Number(props.id), cursor))
// A revisit shows the comments seen last time until the first fresh chunk lands.
const commentItems = computed(() =>
  comments.items.value.length || !comments.loading.value ? comments.items.value : (commentsCache.get(props.id) ?? []),
)
const commentsLoading = computed(() => comments.loading.value && !(!comments.items.value.length && commentsCache.get(props.id)))
watch(comments.loading, (loading) => {
  if (!loading && !comments.error.value) commentsCache.set(props.id, comments.items.value)
})
const sentinel = ref<HTMLElement | null>(null)
let observer: IntersectionObserver | undefined
watch(sentinel, (el) => {
  observer?.disconnect()
  if (!el) return
  observer = new IntersectionObserver((e) => {
    if (e.some((x) => x.isIntersecting)) void comments.loadMore()
  }, { rootMargin: '0px 0px 1600px 0px' })
  observer.observe(el)
})
onBeforeUnmount(() => observer?.disconnect())

watch(
  () => props.id,
  () => {
    comments.reset()
    keptUnread = false
    // The reply draft belongs to the item it was typed for.
    reply.value = ''
    replyError.value = ''
    // A page seen before renders from the cache at once and refreshes quietly.
    const cached = itemCache.get(props.id)
    if (cached) {
      item.value = cached
      state.value = 'ok'
    }
    void load(!!cached)
    void comments.loadMore()
  },
  { immediate: true },
)
watch(item, (it) => {
  if (it) itemCache.set(it.id, it)
  currentItemKind.value = it?.kind ?? ''
  // Page title by kind: a mod page thread or bug report is no «Issue».
  if (it) document.title = 'IssueWatcher · ' + t('title.itemKind.' + it.kind)
})
onBeforeUnmount(() => {
  currentItemKind.value = ''
})

/** Comment thread state: waiting for the owner's answer, answered (owner wrote last) or resolved («Решено», local). */
const threadState = computed(() => {
  const it = item.value
  if (!it || it.kind !== 'comment') return ''
  return it.resolved ? 'resolved' : it.state === 'open' ? 'waiting' : 'answered'
})
const marking = ref(false)
async function markUnread() {
  const it = item.value
  if (!it || marking.value) return
  marking.value = true
  const r = await api.markUnread(it.id)
  marking.value = false
  if (!r.ok) {
    toast.add({ severity: 'error', summary: t('issues.bulkFailed'), detail: itemRef(it), life: 5000 })
    return
  }
  keptUnread = true
  if (String(it.id) === props.id) item.value = { ...it, unread: true }
  void app.loadRepos()
}
async function markReadAgain() {
  const it = item.value
  if (!it || marking.value) return
  marking.value = true
  const r = await api.markRead(it.id)
  marking.value = false
  if (!r.ok) {
    toast.add({ severity: 'error', summary: t('issues.bulkFailed'), detail: itemRef(it), life: 5000 })
    return
  }
  keptUnread = false
  if (String(it.id) === props.id) item.value = { ...it, unread: false }
  void app.loadRepos()
}
// U (physical key, any layout): flips read / unread of the open item.
function onKey(e: KeyboardEvent) {
  if (e.code !== 'KeyU' || e.ctrlKey || e.metaKey || e.altKey || typing(e) || !item.value) return
  e.preventDefault()
  void (item.value.unread ? markReadAgain() : markUnread())
}
window.addEventListener('keydown', onKey)
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
async function setResolved(resolved: boolean) {
  const it = item.value
  if (!it || marking.value) return
  marking.value = true
  const r = await api.setResolved([it.id], resolved)
  marking.value = false
  if (!r.ok) {
    toast.add({ severity: 'error', summary: t('issues.bulkFailed'), detail: itemRef(it), life: 5000 })
    return
  }
  toast.add({ severity: 'success', summary: t(resolved ? 'comments.resolvedToast' : 'comments.reopenedToast', 1), detail: itemRef(it), life: 3000 })
  void load(true)
  void app.loadRepos()
}
watch(
  () => app.dataVersion,
  () => {
    void load(true)
    if (comments.done.value) void comments.loadTail() // new comments land at the end
  },
)

const repo = computed(() => app.repos.find((r) => r.id === item.value?.repoId))
const platform = computed(() => item.value?.platform || 'github')
const mod = computed(() => isModPlatform(platform.value))
const caps = computed(() => app.caps(platform.value))
/** The platform's reply limit (Steam 999 characters, GitHub 65536). */
const maxReply = computed(() => replyLimit(caps.value))
const tooLong = computed(() => replyTooLong(reply.value, maxReply.value))
const pname = computed(() => platformName(platform.value))
/** The account replies go out as: the GitHub login, or the platform's checked account. */
const account = computed(() => (mod.value ? (app.platforms.find((p) => p.id === platform.value)?.account ?? '') : (app.github?.login ?? '')))
/** How the owner is shown: the display name when the account is an id (Steam persona), else the account. */
const selfName = computed(() => {
  if (!mod.value) return account.value
  const p = app.platforms.find((x) => x.id === platform.value)
  return p?.accountName || account.value
})
/** A post's author as shown: the owner by display name (never a raw SteamID), others as synced. */
const authorName = (name: string, mine: boolean) => (mine ? selfName.value || t('item.youTag') : name || t('common.unknown'))
const isComment = computed(() => item.value?.kind === 'comment')
/** Lists this item belongs to: Comments for a mod page thread, else Issues. */
const listRoute = computed(() => (isComment.value ? 'comments' : 'issues'))
const canReply = computed(() => (mod.value ? caps.value.reply : app.githubConnected))
/** A mod page's linked code project (shown in the meta list). */
const codeRepo = computed(() => (mod.value && repo.value?.linkedTo ? app.repos.find((r) => r.id === repo.value?.linkedTo) : undefined))
/**
 * The project whose folder a fix runs in (server-side fixProjectId: the
 * project's own, or for a linked mod page the code project's).
 */
const fixRepo = computed(() => (item.value?.needsLink ? undefined : app.repos.find((r) => r.id === item.value?.fixProjectId)))
/** A fix without a usable folder is disabled (the API refuses it too). */
const noFolder = computed(() => !!item.value && !item.value.fixable)
const fixHint = computed(() => {
  if (item.value?.needsLink) return t('platforms.fixNeedsLink')
  if (mod.value && fixRepo.value) return t('platforms.fixNeedsCodeFolder', { name: fixRepo.value.name })
  return t('folder.needed')
})
const folderOpen = ref(false)
/** Issue flows (a triage is per project: Projects › «Разобрать проект»). */
const ITEM_FLOWS = computed(() => JOB_FLOWS.filter((f) => f !== 'triage' && (f !== 'label' || caps.value.setLabels)))

// The top bar is the page heading: Issues › owner/repo#12.
useCrumbs(() => {
  const it = item.value
  return [
    it?.kind === 'comment' ? { label: t('nav.comments'), to: '/comments' } : { label: t('nav.issues'), to: '/issues' },
    it ? { label: itemRef(it) } : { label: props.id },
  ]
})

async function send() {
  const it = item.value
  const body = reply.value.trim()
  if (!it || !body || sending.value || tooLong.value) return
  sending.value = true
  replyError.value = ''
  const r = await api.reply(it.id, body)
  sending.value = false
  const here = String(it.id) === props.id // still on the item the reply went to
  if (!r.ok) {
    if (!here) {
      toast.add({ severity: 'error', summary: t('item.replyFailed'), detail: itemRef(it), life: 5000 })
      return
    }
    const b = r.body as { code?: string; platform?: string } | undefined
    replyError.value = r.status === 409 && b?.code ? t('replyErrors.' + b.code, { platform: platformName(b.platform || platform.value), limit: num(maxReply.value) }) : r.error
    return
  }
  if (here) {
    item.value = { ...it, comments: it.comments + 1 }
    if (comments.done.value) void comments.loadTail()
    reply.value = ''
  }
  toast.add({ severity: 'success', summary: t('item.replyPosted'), detail: itemRef(it), life: 3000 })
}

function onComposerKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault()
    void send()
  }
}

// --- agent jobs of this item (newest first), live from job.changed
const itemJobs = ref<Job[]>([])
async function loadJobs() {
  const id = props.id
  const r = await api.jobs({ item: Number(id), limit: 5 })
  if (r.ok && id === props.id) itemJobs.value = r.data.items
}
watch(() => props.id, () => {
  itemJobs.value = itemJobsCache.get(props.id) ?? []
  void loadJobs()
}, { immediate: true })
watch(itemJobs, (list) => itemJobsCache.set(props.id, list), { deep: true })
useJobEvents({
  job: (j) => {
    if (j.itemId !== Number(props.id)) return
    const i = itemJobs.value.findIndex((x) => x.id === j.id)
    if (i >= 0) itemJobs.value.splice(i, 1, j)
    else itemJobs.value = [j, ...itemJobs.value].slice(0, 5)
  },
})
const activeJob = (flow: JobFlow) => itemJobs.value.find((j) => j.flow === flow && isActive(j.state))
/** The newest job and unfinished ones show their details (result, Push, log). */
const detailed = (j: Job, i: number) => i === 0 || isActive(j.state)
const logOpen = ref<Record<number, boolean>>({})

const pusher = usePush()
async function push(j: Job) {
  const r = await pusher.push(j)
  if (r) itemJobs.value = itemJobs.value.map((x) => (x.id === r.id ? r : x))
  else void loadJobs() // a failed push leaves the job in needs_review with publishError
}

const jobsStore = useJobsStore()
const report = useDispatchToast()
const dispatching = ref(false)
async function dispatch(flow: JobFlow, profileId?: string) {
  const it = item.value
  if (!it || dispatching.value) return
  dispatching.value = true
  const r = await jobsStore.dispatch([it.id], flow, profileId)
  dispatching.value = false
  report(r)
  if (r.ok) void loadJobs()
  else if (r.status === 409 && (r.body as { code?: string } | undefined)?.code === 'no_folder') void load(true) // mapping changed meanwhile: fresh fixable
}
function profileMenu(flow: JobFlow) {
  const role = jobsStore.roleProfile(flow)
  return jobsStore.profiles.map((p) => ({
    label: p.name + (p.id === role ? ' ✓' : ''),
    icon: p.cli === 'claude' ? 'pi pi-sparkles' : 'pi pi-code',
    command: () => void dispatch(flow, p.id),
  }))
}

const initials = (name: string) => (name || '?').slice(0, 2).toUpperCase()
const avatar = (login: string) => (login && !mod.value ? `https://github.com/${encodeURIComponent(login)}.png?size=64` : '')
</script>

<template>
  <div class="page">
    <template v-if="state === 'loading'">
      <Skeleton
        height="40px"
        width="60%"
      />
      <Skeleton height="320px" />
    </template>

    <EmptyState
      v-else-if="state !== 'ok' || !item"
      :icon="state === 'missing' ? 'pi pi-search' : 'pi pi-exclamation-triangle'"
      :title="state === 'missing' ? t('item.notFound') : state === 'unavailable' ? t('item.unavailable') : t('item.loadError')"
      :text="state === 'missing' ? t('item.missingText') : errorText"
    >
      <Button
        as="router-link"
        :to="currentItemKind === 'comment' ? '/comments' : '/issues'"
        :label="currentItemKind === 'comment' ? t('item.backComments') : t('item.back')"
        icon="pi pi-arrow-left"
        severity="secondary"
        size="small"
      />
    </EmptyState>

    <template v-else>
      <header class="head">
        <div class="head-main">
          <h2 class="title">
            {{ item.title }}
          </h2>
          <div class="head-meta">
            <span
              v-if="threadState"
              class="state-pill"
              :class="threadState === 'waiting' ? 'open' : 'closed'"
            >
              <i :class="threadState === 'waiting' ? 'pi pi-clock' : 'pi pi-check-circle'" />
              {{ t('comments.state.' + threadState) }}
            </span>
            <span
              v-else
              class="state-pill"
              :class="item.state"
            >
              <i :class="item.state === 'closed' ? 'pi pi-check-circle' : 'pi pi-circle'" />
              {{ item.state === 'closed' ? t('item.closed') : t('item.open') }}
            </span>
            <span class="muted">{{ isComment ? t('words.replies', item.comments) : t('words.comments', item.comments) }}</span>
          </div>
        </div>
        <Button
          v-if="threadState"
          :label="threadState === 'resolved' ? t('comments.reopen') : t('comments.resolve')"
          :icon="threadState === 'resolved' ? 'pi pi-undo' : 'pi pi-check'"
          :loading="marking"
          severity="secondary"
          outlined
          size="small"
          @click="setResolved(threadState !== 'resolved')"
        />
        <Button
          v-if="item.state === 'open' && !item.unread"
          v-tooltip.bottom="t('item.markUnreadTip')"
          :label="t('item.markUnread')"
          icon="pi pi-eye-slash"
          :disabled="marking"
          severity="secondary"
          text
          size="small"
          @click="markUnread"
        />
        <Button
          as="a"
          :href="safeUrl(item.url)"
          target="_blank"
          rel="noopener noreferrer"
          :label="t('platforms.openOn', { platform: pname })"
          icon="pi pi-external-link"
          severity="secondary"
          outlined
          size="small"
        />
      </header>

      <div class="layout">
        <div class="thread">
          <article class="post panel">
            <header class="post-head">
              <img
                v-if="avatar(item.author)"
                :src="avatar(item.author)"
                alt=""
                class="av"
                referrerpolicy="no-referrer"
              >
              <span
                v-else
                class="av"
              >{{ initials(authorName(item.author, item.mine)) }}</span>
              <b>{{ authorName(item.author, item.mine) }}</b>
              <span
                v-tooltip.top="absTime(item.createdAt)"
                class="muted"
              >{{ isComment ? t('item.wrote', { time: relTime(item.createdAt) }) : t('item.opened', { time: relTime(item.createdAt) }) }}</span>
              <span
                v-if="!isComment"
                class="author-tag"
              >{{ t('item.authorTag') }}</span>
              <span
                v-if="item.mine"
                class="you-tag"
              >{{ t('item.youTag') }}</span>
            </header>
            <div
              class="post-body"
              :class="{ empty: !item.body }"
            >
              {{ item.body || t('item.noDescription') }}
            </div>
          </article>

          <article
            v-for="c in commentItems"
            :key="c.id"
            class="post panel"
          >
            <header class="post-head">
              <img
                v-if="avatar(c.author)"
                :src="avatar(c.author)"
                alt=""
                class="av"
                referrerpolicy="no-referrer"
              >
              <span
                v-else
                class="av"
              >{{ initials(authorName(c.author, c.mine)) }}</span>
              <b>{{ authorName(c.author, c.mine) }}</b>
              <span
                v-tooltip.top="absTime(c.createdAt)"
                class="muted"
              >{{ isComment ? t('item.replied', { time: relTime(c.createdAt) }) : t('item.commented', { time: relTime(c.createdAt) }) }}</span>
              <span
                v-if="c.author && c.author === item.author && !isComment"
                class="author-tag"
              >{{ t('item.authorTag') }}</span>
              <span
                v-if="c.mine"
                class="you-tag"
              >{{ t('item.youTag') }}</span>
              <a
                v-if="c.url"
                :href="safeUrl(c.url)"
                target="_blank"
                rel="noopener noreferrer"
                class="post-link"
                :aria-label="t('item.openComment', { platform: pname })"
              ><i class="pi pi-external-link" /></a>
            </header>
            <div class="post-body">
              {{ c.body }}
            </div>
          </article>
          <template v-if="commentsLoading">
            <Skeleton
              v-for="i in 2"
              :key="'cs' + i"
              height="96px"
              border-radius="12px"
            />
          </template>
          <div
            ref="sentinel"
            aria-hidden="true"
          />

          <section class="composer panel">
            <div class="composer-head">
              <i class="pi pi-reply" /> {{ t('platforms.replyOn', { platform: pname }) }}
              <span
                v-if="account"
                class="muted"
              >{{ t('item.replyAs', { login: (mod ? '' : '@') + selfName }) }}</span>
              <span class="muted markup">{{ t('platforms.markupHint', { markup: t('platforms.markup.' + platform) }) }}</span>
            </div>
            <Textarea
              v-model="reply"
              auto-resize
              rows="5"
              :maxlength="maxReply"
              :placeholder="t('item.replyPlaceholder')"
              :aria-label="t('item.replyAria')"
              :disabled="!canReply"
              fluid
              @keydown="onComposerKey"
            />
            <small
              v-if="!caps.replyThreaded && canReply"
              class="muted"
            ><i class="pi pi-info-circle" /> {{ t('platforms.replySteamHint', { author: item.author }) }}</small>
            <Message
              v-if="!canReply && mod"
              severity="info"
              size="small"
              variant="simple"
            >
              {{ t('platforms.replyNoCap', { platform: pname }) }}
            </Message>
            <Message
              v-if="tooLong && canReply"
              severity="warn"
              size="small"
              variant="simple"
            >
              {{ t('item.replyTooLong', { platform: pname, limit: num(maxReply) }) }}
            </Message>
            <Message
              v-if="replyError"
              severity="error"
              size="small"
              variant="simple"
            >
              {{ replyError }}
            </Message>
            <div class="composer-foot">
              <span
                class="mono count"
                :class="tooLong ? 'over' : 'muted'"
              >{{ num(replyLength(reply)) }} / {{ num(maxReply) }}</span>
              <Button
                :label="t('item.sendReply')"
                icon="pi pi-send"
                :loading="sending"
                :disabled="!reply.trim() || !canReply || tooLong"
                @click="send"
              />
            </div>
          </section>
        </div>

        <aside class="meta panel">
          <dl>
            <dt>{{ t('item.metaProject') }}</dt>
            <dd>
              <RouterLink :to="{ name: listRoute, query: { repo: String(item.repoId), state: 'all' } }">
                {{ item.repo }}
              </RouterLink>
            </dd>
            <dt>{{ t('item.metaSource') }}</dt>
            <dd class="src">
              <PlatformIcon
                :platform="platform"
                :size="16"
              /> {{ pname }}<template v-if="mod">
                · {{ t('platforms.kindOne.' + (item.kind || 'comment')) }}
              </template>
            </dd>
            <template v-if="mod">
              <dt>{{ t('platforms.linkedCode') }}</dt>
              <dd v-if="codeRepo">
                <PlatformIcon
                  platform="github"
                  :size="14"
                /> <RouterLink :to="{ name: 'issues', query: { repo: String(codeRepo.id), state: 'all' } }">
                  {{ codeRepo.name }}
                </RouterLink>
              </dd>
              <dd
                v-else
                class="muted"
              >
                {{ t('platforms.notLinked') }}
                <Button
                  as="router-link"
                  to="/projects"
                  :label="t('platforms.link')"
                  size="small"
                  link
                  class="folder-link"
                />
              </dd>
            </template>
            <dt v-if="caps.setLabels || item.labels.length">
              {{ t('item.metaLabels') }}
            </dt>
            <dd
              v-if="caps.setLabels || item.labels.length"
              class="labels"
            >
              <LabelTag
                v-for="l in item.labels"
                :key="l"
                :name="l"
              />
              <span
                v-if="!item.labels.length"
                class="muted"
              >{{ t('common.none') }}</span>
            </dd>
            <dt>{{ t('item.metaCreated') }}</dt>
            <dd>{{ absTime(item.createdAt) }}</dd>
            <dt>{{ t('item.metaUpdated') }}</dt>
            <dd>{{ absTime(item.updatedAt) }}</dd>
            <template v-if="item.closedAt">
              <dt>{{ t('item.metaClosed') }}</dt>
              <dd>{{ absTime(item.closedAt) }}</dd>
            </template>
            <dt>{{ t('item.metaFolder') }}</dt>
            <dd
              v-if="item.fixFolder"
              class="mono small"
            >
              {{ item.fixFolder }}
            </dd>
            <dd
              v-else
              class="muted"
            >
              {{ t('item.notMapped') }}
              <Button
                v-if="fixRepo"
                :label="t('folder.link')"
                size="small"
                link
                class="folder-link"
                @click="folderOpen = true"
              />
            </dd>
            <dt>{{ isComment ? t('item.metaThreads') : t('item.metaTotals') }}</dt>
            <dd v-if="repo && isComment">
              <span class="mono">{{ repo.openComments ?? 0 }}</span> {{ t('words.waiting', repo.openComments ?? 0) }} · <span class="mono">{{ repo.closedComments ?? 0 }}</span> {{ t('words.answered', repo.closedComments ?? 0) }}
            </dd>
            <dd v-else-if="repo">
              <span class="mono">{{ repo.open - (repo.openComments ?? 0) }}</span> {{ t('words.open', repo.open - (repo.openComments ?? 0)) }} · <span class="mono">{{ repo.closed - (repo.closedComments ?? 0) }}</span> {{ t('words.closed', repo.closed - (repo.closedComments ?? 0)) }}
            </dd>
            <dd
              v-else
              class="muted"
            >
              —
            </dd>
          </dl>
          <div class="agent">
            <div class="agent-title">
              {{ t('item.agentTitle') }}
            </div>
            <div
              v-for="(j, i) in itemJobs"
              :key="j.id"
              class="job-block"
            >
              <RouterLink
                :to="'/jobs/' + j.id"
                class="job-card"
              >
                <JobBadge
                  :state="j.state"
                  :flow="j.flow"
                  :outcome="jobOutcome(j)"
                />
                <span class="job-card-text">
                  <span>{{ t('jobs.flow.' + j.flow) }}<template v-if="j.phase && isActive(j.state) && j.state !== 'running'"> · {{ t('jobs.phase.' + j.phase) }}</template><template v-if="j.origin === 'rule'"> · {{ t('jobs.origin.rule') }}</template></span>
                  <span
                    v-if="j.flow === 'label' && j.result.labels"
                    class="muted small"
                  >{{ j.result.labels.length ? j.result.labels.join(', ') : t('job.labels.none') }}</span>
                  <JobProgress
                    v-if="j.state === 'running'"
                    :id="j.id"
                    :attempt="j.attempt"
                    :started-at="j.startedAt"
                    :phase="j.phase"
                  />
                  <span
                    v-else
                    class="muted small"
                  >{{ relTime(j.createdAt) }}<template v-if="j.attempt > 1"> · ×{{ j.attempt }}</template></span>
                </span>
                <i class="pi pi-angle-right muted" />
              </RouterLink>
              <template v-if="detailed(j, i)">
                <DirectResult
                  v-if="isDirect(j) && (j.result.local || j.state === 'failed')"
                  :job="j"
                />
                <Message
                  v-if="j.result.publishError"
                  severity="error"
                  size="small"
                >
                  {{ t('job.publishError', { error: j.result.publishError }) }}
                </Message>
                <div class="job-actions">
                  <Button
                    v-if="canPush(j)"
                    :label="t('job.actions.push')"
                    icon="pi pi-upload"
                    size="small"
                    :loading="pusher.busy.value"
                    :disabled="pusher.busy.value"
                    @click="push(j)"
                  />
                  <Button
                    :label="logOpen[j.id] ? t('job.log.hide') : t('job.log.show')"
                    :icon="logOpen[j.id] ? 'pi pi-chevron-up' : 'pi pi-list'"
                    size="small"
                    severity="secondary"
                    text
                    :aria-expanded="!!logOpen[j.id]"
                    @click="logOpen[j.id] = !logOpen[j.id]"
                  />
                </div>
                <JobLog
                  v-if="logOpen[j.id]"
                  :job-id="j.id"
                  :attempt="j.attempt"
                />
              </template>
            </div>
            <template
              v-for="f in ITEM_FLOWS"
              :key="f"
            >
              <SplitButton
                :label="t('item.' + f + 'WithAgent')"
                :icon="FLOW_ICON[f]"
                :model="profileMenu(f)"
                :disabled="!!activeJob(f) || dispatching || (f === 'fix' && noFolder)"
                :severity="f === 'fix' ? undefined : 'secondary'"
                :title="activeJob(f) ? t('item.jobRunning') : f === 'fix' && noFolder ? fixHint : undefined"
                class="agent-btn"
                @click="dispatch(f)"
              />
              <div
                v-if="f === 'fix' && noFolder"
                class="folder-hint"
              >
                <i class="pi pi-folder" /> {{ fixHint }}
                <Button
                  v-if="fixRepo"
                  :label="t('folder.link')"
                  size="small"
                  link
                  class="folder-link"
                  @click="folderOpen = true"
                />
              </div>
            </template>
            <FolderDialog
              v-model:visible="folderOpen"
              :project="fixRepo ?? null"
              @saved="load(true)"
            />
          </div>
        </aside>
      </div>
    </template>
  </div>
</template>

<style scoped>
.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 8px 16px;
}

/* Title first, the actions (Решено, Непрочитано, open on the platform) packed at the end. */
.head > .head-main {
  flex: 1 1 auto;
  min-width: 0;
}

.head > :not(.head-main) {
  flex: none;
}

.title {
  font-size: calc(26px * var(--iw-fs, 1));
  line-height: 32px;
  font-weight: 650;
  letter-spacing: -0.01em;
}

.head-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 10px;
}

.layout {
  display: grid;
  grid-template-columns: minmax(0, 2fr) minmax(260px, 1fr);
  gap: 20px;
  align-items: start;
}

.thread {
  display: flex;
  flex-direction: column;
  gap: 14px;
  min-width: 0;
}

.post {
  overflow: hidden;
}

.post-head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 16px;
  font-size: calc(13px * var(--iw-fs, 1));
  background: var(--iw-elevated);
  border-bottom: 1px solid var(--iw-border);
}

.av {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border-radius: 50%;
  font-size: calc(10px * var(--iw-fs, 1));
  font-weight: 700;
  background: var(--iw-hover);
  object-fit: cover;
}

.author-tag,
.you-tag {
  padding: 0 8px;
  border-radius: 999px;
  font-size: calc(11px * var(--iw-fs, 1));
  border: 1px solid var(--iw-border-strong);
  color: var(--iw-muted);
}

.you-tag {
  color: var(--iw-primary);
  border-color: color-mix(in srgb, var(--iw-primary) 40%, transparent);
}

.post-link {
  margin-left: auto;
  color: var(--iw-muted);
}

.post-body {
  padding: 14px 16px 16px;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  line-height: 1.6;
}

.post-body.empty {
  color: var(--iw-dimmed);
  font-style: italic;
}

.composer {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 14px 16px 16px;
}

.composer-head {
  display: flex;
  align-items: center;
  gap: 8px;
  font-weight: 600;
}

.composer-head .muted {
  font-weight: 400;
}

.composer-head .markup {
  margin-left: auto;
  font-size: calc(12px * var(--iw-fs, 1));
}

.composer-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.count {
  font-size: calc(12px * var(--iw-fs, 1));
}

.count.over {
  color: var(--iw-danger);
}

.meta {
  position: sticky;
  top: calc(var(--iw-topbar) + 20px);
  padding: 18px 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

dl {
  margin: 0;
  display: grid;
  grid-template-columns: 1fr;
  gap: 4px;
}

dt {
  margin-top: 10px;
  font-size: calc(11.5px * var(--iw-fs, 1));
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--iw-dimmed);
}

dt:first-child {
  margin-top: 0;
}

dd {
  margin: 0;
}

.small {
  font-size: calc(12px * var(--iw-fs, 1));
}

.src {
  display: flex;
  align-items: center;
  gap: 8px;
}

.labels {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.agent {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding-top: 14px;
  border-top: 1px solid var(--iw-border);
}

.agent-title {
  font-size: calc(11.5px * var(--iw-fs, 1));
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--iw-dimmed);
}

.job-block {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}

.job-actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}

.job-card {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border-radius: var(--iw-radius-sm);
  border: 1px solid var(--iw-border);
  color: var(--iw-text);
}

.job-card:hover {
  background: var(--iw-hover);
  color: var(--iw-text);
}

.job-card-text {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-width: 0;
}

.agent-btn {
  width: 100%;
}

.agent-btn :deep(.p-splitbutton-button) {
  flex: 1;
}

.folder-hint {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 6px;
  margin-top: -2px;
  font-size: calc(12.5px * var(--iw-fs, 1));
  color: var(--iw-warn);
}

.folder-link {
  padding: 0 2px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

@media (width <= 1023px) {
  .layout {
    grid-template-columns: minmax(0, 1fr);
  }

  .meta {
    position: static;
  }
}
</style>
