<script setup lang="ts">
import { computed, reactive, ref, shallowReactive, shallowRef, watch } from 'vue'
import Button from 'primevue/button'
import Skeleton from 'primevue/skeleton'
import Message from 'primevue/message'
import { useI18n } from 'vue-i18n'
import { api } from '../api/client'
import { highlightLines, highlighter, languageOf, parseDiff, type DiffFile } from '../lib/diff'
import { num } from '../lib/format'

// Unified diff of one attempt: file list with +/− counts, then one block per
// file (collapsed when big, so a 5 MB diff renders only what is opened),
// line numbers, +/− colouring and per-line syntax highlighting.
const props = defineProps<{ jobId: number; attempt: number; version: string; truncated?: boolean }>()
const { t } = useI18n()

const BIG_FILE = 400 // lines: collapsed by default
const AUTO_BUDGET = 2500 // lines opened automatically, in file order

const files = shallowRef<DiffFile[]>([])
const loading = ref(false)
const error = ref('')
const open = reactive(new Set<string>())
/** Highlighted HTML per file path (per line, hunk headers excluded). */
const html = shallowReactive(new Map<string, string[][]>())

let gen = 0
async function load() {
  const g = ++gen
  loading.value = true
  const r = await api.jobDiff(props.jobId, props.attempt)
  if (g !== gen) return
  loading.value = false
  if (!r.ok) {
    error.value = r.error
    files.value = []
    return
  }
  error.value = ''
  const parsed = parseDiff(r.data)
  files.value = parsed
  open.clear()
  html.clear()
  let budget = AUTO_BUDGET
  for (const f of parsed) {
    if (f.size <= BIG_FILE && f.size <= budget) {
      budget -= f.size
      open.add(f.path)
      void paint(f)
    }
  }
}
watch(() => [props.jobId, props.attempt, props.version], load, { immediate: true })

async function paint(f: DiffFile) {
  if (html.has(f.path) || f.binary) return
  const lang = languageOf(f.path)
  const hljs = lang ? await highlighter(lang) : null
  if (!hljs) return
  const g = gen
  // Yield to the browser between files so a long diff keeps the page responsive.
  await new Promise((r) => requestAnimationFrame(r))
  if (g !== gen) return
  html.set(f.path, f.hunks.map((h) => highlightLines(hljs, lang, h.lines.map((l) => (l.kind === 'meta' ? '' : l.text)))))
}

function toggle(f: DiffFile) {
  if (open.has(f.path)) open.delete(f.path)
  else {
    open.add(f.path)
    void paint(f)
  }
}

function jump(f: DiffFile) {
  if (!open.has(f.path)) toggle(f)
  document.getElementById('diff-' + f.path)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

const totals = computed(() => files.value.reduce((a, f) => ({ added: a.added + f.added, deleted: a.deleted + f.deleted }), { added: 0, deleted: 0 }))
const STATUS_ICON: Record<DiffFile['status'], string> = { A: 'pi pi-plus-circle', M: 'pi pi-pencil', D: 'pi pi-minus-circle', R: 'pi pi-arrow-right' }
</script>

<template>
  <div class="iw-diff">
    <template v-if="loading && !files.length">
      <Skeleton height="28px" />
      <Skeleton height="160px" />
    </template>
    <Message
      v-else-if="error"
      severity="error"
      size="small"
    >
      {{ error }}
    </Message>
    <p
      v-else-if="!files.length"
      class="muted empty"
    >
      {{ t('job.diff.none') }}
    </p>
    <template v-else>
      <Message
        v-if="truncated"
        severity="warn"
        size="small"
      >
        {{ t('job.diff.truncated') }}
      </Message>
      <ul class="file-list">
        <li
          v-for="f in files"
          :key="f.path"
        >
          <button
            type="button"
            class="file-link"
            @click="jump(f)"
          >
            <i
              :class="[STATUS_ICON[f.status], 's-' + f.status.toLowerCase()]"
              :title="t('job.diff.status.' + f.status)"
            />
            <span class="mono path">{{ f.status === 'R' ? f.oldPath + ' → ' + f.path : f.path }}</span>
            <span
              v-if="f.binary"
              class="muted"
            >{{ t('job.diff.binary') }}</span>
            <template v-else>
              <span class="mono plus">+{{ num(f.added) }}</span>
              <span class="mono minus">−{{ num(f.deleted) }}</span>
            </template>
          </button>
        </li>
      </ul>
      <div class="totals muted">
        {{ t('job.diff.totals', { files: files.length }) }}
        <span class="mono plus">+{{ num(totals.added) }}</span>
        <span class="mono minus">−{{ num(totals.deleted) }}</span>
      </div>

      <section
        v-for="f in files"
        :id="'diff-' + f.path"
        :key="f.path"
        class="file"
      >
        <button
          type="button"
          class="file-head"
          :aria-expanded="open.has(f.path)"
          @click="toggle(f)"
        >
          <i :class="open.has(f.path) ? 'pi pi-chevron-down' : 'pi pi-chevron-right'" />
          <span class="mono path">{{ f.status === 'R' ? f.oldPath + ' → ' + f.path : f.path }}</span>
          <span class="mono plus">+{{ num(f.added) }}</span>
          <span class="mono minus">−{{ num(f.deleted) }}</span>
          <span
            v-if="!open.has(f.path) && f.size > BIG_FILE"
            class="muted big"
          >{{ t('job.diff.bigFile', { n: num(f.size) }) }}</span>
        </button>
        <div
          v-if="open.has(f.path)"
          class="code mono"
        >
          <div
            v-if="f.binary || !f.hunks.length"
            class="line meta"
          >
            <span class="no" /><span class="no" /><span class="txt">{{ f.binary ? t('job.diff.binary') : t('job.diff.noText') }}</span>
          </div>
          <template
            v-for="(h, hi) in f.hunks"
            :key="hi"
          >
            <div class="line hunk">
              <span class="no" /><span class="no" /><span class="txt">{{ h.header }}</span>
            </div>
            <div
              v-for="(l, li) in h.lines"
              :key="li"
              class="line"
              :class="l.kind"
            >
              <span class="no">{{ l.old ?? '' }}</span><span class="no">{{ l.new ?? '' }}</span>
              <!-- highlight.js output: the highlighter HTML-escapes the source text -->
              <!-- eslint-disable vue/no-v-html -->
              <span
                v-if="html.get(f.path)?.[hi]?.[li] && l.kind !== 'meta'"
                class="txt"
                v-html="html.get(f.path)?.[hi]?.[li]"
              />
              <!-- eslint-enable vue/no-v-html -->
              <span
                v-else
                class="txt"
              >{{ l.text }}</span>
            </div>
          </template>
        </div>
      </section>
      <Button
        v-if="loading"
        :label="t('job.diff.loading')"
        loading
        text
        size="small"
      />
    </template>
  </div>
</template>

<style scoped>
.iw-diff {
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-width: 0;
}

.empty {
  margin: 0;
}

.file-list {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
}

.file-link,
.file-head {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 4px 6px;
  border: 0;
  border-radius: 6px;
  background: none;
  color: var(--iw-text);
  font: inherit;
  font-size: calc(13px * var(--iw-fs, 1));
  text-align: left;
  cursor: pointer;
}

.file-link:hover {
  background: var(--iw-hover);
}

.path {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  flex: 1;
}

.plus {
  color: var(--iw-success);
}

.minus {
  color: var(--iw-danger);
}

.s-a {
  color: var(--iw-success);
}

.s-d {
  color: var(--iw-danger);
}

.s-m,
.s-r {
  color: var(--iw-warn);
}

.totals {
  display: flex;
  gap: 10px;
  font-size: calc(12.5px * var(--iw-fs, 1));
}

.file {
  border: 1px solid var(--iw-border);
  border-radius: var(--iw-radius-sm);
  overflow: hidden;
  scroll-margin-top: calc(var(--iw-topbar) + 12px);
}

.file-head {
  padding: 8px 12px;
  border-radius: 0;
  background: var(--iw-elevated);
  position: sticky;
  top: 0;
}

.big {
  font-size: calc(12px * var(--iw-fs, 1));
}

.code {
  overflow-x: auto;
  font-size: calc(12.5px * var(--iw-fs, 1));
  line-height: 1.55;
}

.line {
  display: grid;
  grid-template-columns: 52px 52px minmax(0, max-content);
  min-width: 100%;
  width: max-content;
}

.no {
  padding: 0 8px;
  text-align: right;
  color: var(--iw-dimmed);
  user-select: none;
  border-right: 1px solid var(--iw-border);
}

.txt {
  padding: 0 12px;
  white-space: pre;
}

.line.add {
  background: var(--iw-success-soft);
}

.line.add .txt::before {
  content: '+';
  margin-left: -10px;
  margin-right: 2px;
  color: var(--iw-success);
}

.line.del {
  background: var(--iw-danger-soft);
}

.line.del .txt::before {
  content: '−';
  margin-left: -10px;
  margin-right: 2px;
  color: var(--iw-danger);
}

.line.hunk {
  color: var(--iw-primary);
  background: var(--iw-primary-soft);
}

.line.meta {
  color: var(--iw-muted);
  font-style: italic;
}
</style>

<style>
/* highlight.js token colours on the app tokens (dark default, light below). */
/* stylelint-disable selector-class-pattern -- highlight.js class names */
.iw-diff .hljs-keyword,
.iw-diff .hljs-selector-tag,
.iw-diff .hljs-built_in,
.iw-diff .hljs-meta .hljs-keyword {
  color: #c792ea;
}

.iw-diff .hljs-string,
.iw-diff .hljs-regexp,
.iw-diff .hljs-addition,
.iw-diff .hljs-attribute {
  color: #a5d6a7;
}

.iw-diff .hljs-number,
.iw-diff .hljs-literal,
.iw-diff .hljs-symbol,
.iw-diff .hljs-variable.language_ {
  color: #f1bb69;
}

.iw-diff .hljs-comment,
.iw-diff .hljs-quote {
  color: var(--iw-dimmed);
  font-style: italic;
}

.iw-diff .hljs-title,
.iw-diff .hljs-title.function_,
.iw-diff .hljs-section {
  color: #82aaff;
}

.iw-diff .hljs-type,
.iw-diff .hljs-title.class_,
.iw-diff .hljs-name,
.iw-diff .hljs-selector-class {
  color: #55c7d4;
}

.iw-diff .hljs-attr,
.iw-diff .hljs-property,
.iw-diff .hljs-params {
  color: #e8b97a;
}

.iw-diff .hljs-meta,
.iw-diff .hljs-tag {
  color: var(--iw-muted);
}

:root:not(.dark) .iw-diff .hljs-keyword,
:root:not(.dark) .iw-diff .hljs-selector-tag,
:root:not(.dark) .iw-diff .hljs-built_in {
  color: #8839ef;
}

:root:not(.dark) .iw-diff .hljs-string,
:root:not(.dark) .iw-diff .hljs-regexp,
:root:not(.dark) .iw-diff .hljs-attribute {
  color: #2d7d2d;
}

:root:not(.dark) .iw-diff .hljs-number,
:root:not(.dark) .iw-diff .hljs-literal,
:root:not(.dark) .iw-diff .hljs-symbol {
  color: #b15c00;
}

:root:not(.dark) .iw-diff .hljs-title,
:root:not(.dark) .iw-diff .hljs-title.function_,
:root:not(.dark) .iw-diff .hljs-section {
  color: #1e66f5;
}

:root:not(.dark) .iw-diff .hljs-type,
:root:not(.dark) .iw-diff .hljs-title.class_,
:root:not(.dark) .iw-diff .hljs-name {
  color: #0b7285;
}

:root:not(.dark) .iw-diff .hljs-attr,
:root:not(.dark) .iw-diff .hljs-property,
:root:not(.dark) .iw-diff .hljs-params {
  color: #9a5b00;
}
</style>
