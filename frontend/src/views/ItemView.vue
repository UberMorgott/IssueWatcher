<script setup lang="ts">
import { computed, ref, watch } from 'vue'
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
import { useAppStore } from '../stores/app'
import { absTime, relTime, shortRepo } from '../lib/format'

const props = defineProps<{ id: string }>()
const app = useAppStore()
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

watch(() => props.id, () => load(), { immediate: true })
watch(() => app.dataVersion, () => load(true))

const repo = computed(() => app.repos.find((r) => r.id === item.value?.repoId))

async function send() {
  const it = item.value
  const body = reply.value.trim()
  if (!it || !body || sending.value) return
  sending.value = true
  replyError.value = ''
  const r = await api.reply(it.id, body)
  sending.value = false
  if (!r.ok) {
    replyError.value = r.status === 409 ? 'Not signed in to GitHub — reconnect on the Connections page.' : r.error
    return
  }
  item.value = { ...it, comments: it.comments + 1, commentsList: [...it.commentsList, r.data] }
  reply.value = ''
  toast.add({ severity: 'success', summary: 'Reply posted', detail: `${it.repo}#${it.number}`, life: 3000 })
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
    <nav
      class="crumbs"
      aria-label="Breadcrumb"
    >
      <RouterLink to="/issues">
        Issues
      </RouterLink>
      <i class="pi pi-angle-right" />
      <template v-if="item">
        <RouterLink :to="{ name: 'issues', query: { repo: String(item.repoId), state: 'all' } }">
          {{ item.repo }}
        </RouterLink>
        <i class="pi pi-angle-right" />
        <span class="mono">#{{ item.number }}</span>
      </template>
      <span
        v-else
        class="mono"
      >{{ id }}</span>
    </nav>

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
      :title="state === 'missing' ? 'Issue not found' : state === 'unavailable' ? 'Issue details are not available yet' : 'Could not load this issue'"
      :text="state === 'missing' ? 'It may belong to a project that is no longer synced.' : errorText"
    >
      <Button
        as="router-link"
        to="/issues"
        label="Back to issues"
        icon="pi pi-arrow-left"
        severity="secondary"
        size="small"
      />
    </EmptyState>

    <template v-else>
      <header class="head">
        <div class="head-main">
          <h2 class="title">
            {{ item.title }} <span class="num mono">#{{ item.number }}</span>
          </h2>
          <div class="head-meta">
            <span
              class="state-pill"
              :class="item.state"
            >
              <i :class="item.state === 'closed' ? 'pi pi-check-circle' : 'pi pi-circle'" />
              {{ item.state === 'closed' ? 'Closed' : 'Open' }}
            </span>
            <span class="muted"><b>{{ item.author || 'unknown' }}</b> opened this <span v-tooltip.bottom="absTime(item.createdAt)">{{ relTime(item.createdAt) }}</span> · {{ item.comments }} comments</span>
          </div>
        </div>
        <Button
          as="a"
          :href="item.url"
          target="_blank"
          rel="noopener noreferrer"
          label="Open on GitHub"
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
              <b>{{ item.author || 'unknown' }}</b>
              <span class="muted">opened {{ relTime(item.createdAt) }}</span>
              <span class="author-tag">author</span>
            </header>
            <div
              class="post-body"
              :class="{ empty: !item.body }"
            >
              {{ item.body || 'No description provided.' }}
            </div>
          </article>

          <article
            v-for="c in item.commentsList"
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
              <b>{{ c.author || 'unknown' }}</b>
              <span
                v-tooltip.top="absTime(c.createdAt)"
                class="muted"
              >commented {{ relTime(c.createdAt) }}</span>
              <span
                v-if="c.author && c.author === item.author"
                class="author-tag"
              >author</span>
              <span
                v-if="c.author && c.author === app.github?.login"
                class="you-tag"
              >you</span>
              <a
                v-if="c.url"
                :href="c.url"
                target="_blank"
                rel="noopener noreferrer"
                class="post-link"
                aria-label="Open comment on GitHub"
              ><i class="pi pi-external-link" /></a>
            </header>
            <div class="post-body">
              {{ c.body }}
            </div>
          </article>

          <section class="composer panel">
            <div class="composer-head">
              <i class="pi pi-reply" /> Reply on GitHub
              <span
                v-if="app.github?.login"
                class="muted"
              >as @{{ app.github.login }}</span>
            </div>
            <Textarea
              v-model="reply"
              auto-resize
              rows="5"
              :maxlength="MAX_REPLY"
              placeholder="Write a reply…  (Ctrl+Enter to send)"
              aria-label="Reply"
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
              <span class="muted mono count">{{ reply.length.toLocaleString() }} / {{ MAX_REPLY.toLocaleString() }}</span>
              <Button
                label="Send reply"
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
            <dt>Project</dt>
            <dd>
              <RouterLink :to="{ name: 'issues', query: { repo: String(item.repoId), state: 'all' } }">
                {{ shortRepo(item.repo) }}
              </RouterLink>
              <div class="muted small">
                {{ item.repo }}
              </div>
            </dd>
            <dt>Source</dt>
            <dd class="src">
              <PlatformIcon
                platform="github"
                :size="16"
              /> GitHub
            </dd>
            <dt>Labels</dt>
            <dd class="labels">
              <LabelTag
                v-for="l in item.labels"
                :key="l"
                :name="l"
              />
              <span
                v-if="!item.labels.length"
                class="muted"
              >None</span>
            </dd>
            <dt>Created</dt>
            <dd>{{ absTime(item.createdAt) }}</dd>
            <dt>Updated</dt>
            <dd>{{ absTime(item.updatedAt) }}</dd>
            <template v-if="item.closedAt">
              <dt>Closed</dt>
              <dd>{{ absTime(item.closedAt) }}</dd>
            </template>
            <dt>Local folder</dt>
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
              Not mapped
            </dd>
            <dt>Project totals</dt>
            <dd v-if="repo">
              <span class="mono">{{ repo.open }}</span> open · <span class="mono">{{ repo.closed }}</span> closed
            </dd>
            <dd
              v-else
              class="muted"
            >
              —
            </dd>
          </dl>
          <span v-tooltip.top="'Coming in Phase 2'">
            <Button
              label="Send to agent"
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
.crumbs {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: var(--iw-muted);
}

.crumbs i {
  font-size: 11px;
}

.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.title {
  font-size: 26px;
  line-height: 32px;
  font-weight: 650;
  letter-spacing: -0.01em;
}

.num {
  color: var(--iw-dimmed);
  font-weight: 400;
}

.head-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 10px;
}

.head-meta b {
  color: var(--iw-text);
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
  font-size: 13px;
  background: var(--iw-elevated);
  border-bottom: 1px solid var(--iw-border);
}

.av {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border-radius: 50%;
  font-size: 10px;
  font-weight: 700;
  background: var(--iw-hover);
  object-fit: cover;
}

.author-tag,
.you-tag {
  padding: 0 8px;
  border-radius: 999px;
  font-size: 11px;
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
  font-size: 12px;
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
  font-size: 11.5px;
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
  font-size: 12px;
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
