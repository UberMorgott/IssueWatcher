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
import type { IssueDetail } from '../api/types'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '../stores/app'
import { absTime, num, relTime } from '../lib/format'
import { useCrumbs } from '../lib/crumbs'
import { useChunks } from '../lib/chunks'

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

// The top bar is the page heading: Issues › owner/repo#12.
useCrumbs(() => {
  const it = item.value
  return [
    { label: t('nav.issues'), to: '/issues' },
    it ? { label: `${it.repo}#${it.number}` } : { label: props.id },
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
    replyError.value = r.status === 409 ? t('item.notSignedIn') : r.error
    return
  }
  item.value = { ...it, comments: it.comments + 1 }
  if (comments.done.value) void comments.loadTail()
  reply.value = ''
  toast.add({ severity: 'success', summary: t('item.replyPosted'), detail: `${it.repo}#${it.number}`, life: 3000 })
}

function onComposerKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault()
    void send()
  }
}

const initials = (name: string) => (name || '?').slice(0, 2).toUpperCase()
const avatar = (login: string) => (login ? `https://github.com/${encodeURIComponent(login)}.png?size=64` : '')
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
          :label="t('item.openOnGithub')"
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
                v-if="c.author && c.author === app.github?.login"
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
              <i class="pi pi-reply" /> {{ t('item.replyOnGithub') }}
              <span
                v-if="app.github?.login"
                class="muted"
              >{{ t('item.replyAs', { login: '@' + app.github.login }) }}</span>
            </div>
            <Textarea
              v-model="reply"
              auto-resize
              rows="5"
              :maxlength="MAX_REPLY"
              :placeholder="t('item.replyPlaceholder')"
              :aria-label="t('item.replyAria')"
              :disabled="!app.githubConnected"
              fluid
              @keydown="onComposerKey"
            />
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
                :disabled="!reply.trim() || !app.githubConnected"
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
                platform="github"
                :size="16"
              /> GitHub
            </dd>
            <dt>{{ t('item.metaLabels') }}</dt>
            <dd class="labels">
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
              v-if="repo?.localPath"
              class="mono small"
            >
              {{ repo.localPath }}
            </dd>
            <dd
              v-else
              class="muted"
            >
              {{ t('item.notMapped') }}
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
          <span v-tooltip.top="t('item.agentSoon')">
            <Button
              :label="t('item.sendToAgent')"
              icon="pi pi-sparkles"
              disabled
              fluid
              severity="secondary"
            />
          </span>
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

@media (width <= 1023px) {
  .layout {
    grid-template-columns: minmax(0, 1fr);
  }

  .meta {
    position: static;
  }
}
</style>
