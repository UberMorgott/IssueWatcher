<script setup lang="ts">
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
import { isModPlatform, itemRef, platformName } from '../lib/platforms'

const props = defineProps<{ id: string }>()
const app = useAppStore()
const { t } = useI18n()
const toast = useToast()

const MAX_REPLY = 65536 // GitHub comment body limit (characters)

const item = ref<IssueDetail | null>(null)
const state = ref<'loading' | 'ok' | 'missing' | 'unavailable' | 'error'>('loading')
const errorText = ref('')
const reply = ref('')
const sending = ref(false)
const replyError = ref('')

async function load(quiet = false) {
  if (!quiet) state.value = 'loading'
  const r = await api.issue(props.id)
  if (!r.ok) {
    state.value = r.status === 404 ? (r.error === 'not found' ? 'missing' : 'unavailable') : r.status === 400 ? 'missing' : 'error'
    errorText.value = r.error
    return
  }
  item.value = r.data
  state.value = 'ok'
  // Steam has no threads: a reply is a new comment addressed to the author.
  if (!reply.value && !app.caps(r.data.platform || 'github').replyThreaded && r.data.author) reply.value = '@' + r.data.author + ' '
  if (r.data.unread) {
    const m = await api.markRead(r.data.id)
    if (m.ok) {
      item.value = { ...r.data, unread: false }
      void app.loadRepos()
    }
  }
}

// Comments arrive in chunks while the reader scrolls towards the end (sentinel
// ~2 screens ahead); skeleton posts mark the loading tail.
const comments = useChunks((cursor) => api.comments(Number(props.id), cursor))
const commentItems = comments.items
const commentsLoading = comments.loading
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
    void load()
    void comments.loadMore()
  },
  { immediate: true },
)
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
const pname = computed(() => platformName(platform.value))
/** The account replies go out as: the GitHub login, or the platform's checked account. */
const account = computed(() => (mod.value ? (app.platforms.find((p) => p.id === platform.value)?.account ?? '') : (app.github?.login ?? '')))
const canReply = computed(() => (mod.value ? caps.value.reply : app.githubConnected))
/** A mod page's linked code project (its folder runs the mod's fixes). */
const codeRepo = computed(() => (mod.value && repo.value?.linkedTo ? app.repos.find((r) => r.id === repo.value?.linkedTo) : undefined))
/** The folder a fix runs in: the project's own, or for a mod page the linked code project's. */
const fixRepo = computed(() => (mod.value ? codeRepo.value : repo.value))
/** A fix without a folder is disabled (the API refuses it too). */
const noFolder = computed(() => (mod.value ? !codeRepo.value?.localPath : !!repo.value && !repo.value.localPath))
const fixHint = computed(() => {
  if (!mod.value) return t('folder.needed')
  return codeRepo.value ? t('platforms.fixNeedsCodeFolder', { name: codeRepo.value.name }) : t('platforms.fixNeedsLink')
})
const folderOpen = ref(false)
/** Issue flows (a triage is per project: Projects › «Запустить проект»). */
const ITEM_FLOWS = computed(() => JOB_FLOWS.filter((f) => f !== 'triage' && (f !== 'label' || caps.value.setLabels)))

// The top bar is the page heading: Issues › owner/repo#12.
useCrumbs(() => {
  const it = item.value
  return [
    { label: t('nav.issues'), to: '/issues' },
    it ? { label: itemRef(it) } : { label: props.id },
  ]
})

async function send() {
  const it = item.value
  const body = reply.value.trim()
  if (!it || !body || sending.value) return
  sending.value = true
  replyError.value = ''
  const r = await api.reply(it.id, body)
  sending.value = false
  if (!r.ok) {
    const b = r.body as { code?: string; platform?: string } | undefined
    replyError.value = r.status === 409 && b?.code ? t('replyErrors.' + b.code, { platform: platformName(b.platform || platform.value) }) : r.error
    return
  }
  item.value = { ...it, comments: it.comments + 1 }
  if (comments.done.value) void comments.loadTail()
  reply.value = ''
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
  const r = await api.jobs({ item: Number(props.id), limit: 5 })
  if (r.ok) itemJobs.value = r.data.items
}
watch(() => props.id, () => {
  itemJobs.value = []
  void loadJobs()
}, { immediate: true })
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
  else if (r.status === 409 && (r.body as { code?: string } | undefined)?.code === 'no_folder') void app.loadRepos() // mapping changed meanwhile
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
        to="/issues"
        :label="t('item.back')"
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
              class="state-pill"
              :class="item.state"
            >
              <i :class="item.state === 'closed' ? 'pi pi-check-circle' : 'pi pi-circle'" />
              {{ item.state === 'closed' ? t('item.closed') : t('item.open') }}
            </span>
            <span class="muted">{{ t('words.comments', item.comments) }}</span>
          </div>
        </div>
        <Button
          as="a"
          :href="item.url"
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
              >{{ initials(item.author) }}</span>
              <b>{{ item.author || t('common.unknown') }}</b>
              <span class="muted">{{ t('item.opened', { time: relTime(item.createdAt) }) }}</span>
              <span class="author-tag">{{ t('item.authorTag') }}</span>
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
              >{{ initials(c.author) }}</span>
              <b>{{ c.author || t('common.unknown') }}</b>
              <span
                v-tooltip.top="absTime(c.createdAt)"
                class="muted"
              >{{ t('item.commented', { time: relTime(c.createdAt) }) }}</span>
              <span
                v-if="c.author && c.author === item.author"
                class="author-tag"
              >{{ t('item.authorTag') }}</span>
              <span
                v-if="c.author && c.author === account"
                class="you-tag"
              >{{ t('item.youTag') }}</span>
              <a
                v-if="c.url"
                :href="c.url"
                target="_blank"
                rel="noopener noreferrer"
                class="post-link"
                :aria-label="t('item.openComment')"
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
              >{{ t('item.replyAs', { login: (mod ? '' : '@') + account }) }}</span>
              <span class="muted markup">{{ t('platforms.markupHint', { markup: t('platforms.markup.' + platform) }) }}</span>
            </div>
            <Textarea
              v-model="reply"
              auto-resize
              rows="5"
              :maxlength="MAX_REPLY"
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
              v-if="replyError"
              severity="error"
              size="small"
              variant="simple"
            >
              {{ replyError }}
            </Message>
            <div class="composer-foot">
              <span class="muted mono count">{{ num(reply.length) }} / {{ num(MAX_REPLY) }}</span>
              <Button
                :label="t('item.sendReply')"
                icon="pi pi-send"
                :loading="sending"
                :disabled="!reply.trim() || !canReply"
                @click="send"
              />
            </div>
          </section>
        </div>

        <aside class="meta panel">
          <dl>
            <dt>{{ t('item.metaProject') }}</dt>
            <dd>
              <RouterLink :to="{ name: 'issues', query: { repo: String(item.repoId), state: 'all' } }">
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
              v-if="fixRepo?.localPath"
              class="mono small"
            >
              {{ fixRepo.localPath }}
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
            <dt>{{ t('item.metaTotals') }}</dt>
            <dd v-if="repo">
              <span class="mono">{{ repo.open }}</span> {{ t('words.open', repo.open) }} · <span class="mono">{{ repo.closed }}</span> {{ t('words.closed', repo.closed) }}
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
  gap: 16px;
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
