<script setup lang="ts">
import { computed } from 'vue'
import Checkbox from 'primevue/checkbox'
import Message from 'primevue/message'
import { useI18n } from 'vue-i18n'
import PlatformIcon from './PlatformIcon.vue'
import type { CheckResult, CheckTargetPlan, PlanTarget } from '../api/types'
import { bytes, noteText, refusalText, requestText, shortSha, smokeKindText, stepLabel, targetBlock, targetLabel } from '../lib/release'
import { elapsed } from '../lib/format'
import { safeUrl } from '../lib/safeUrl'

// A release dry run (POST …/release/plan, …/publish-profile/check): refusals,
// version, changelog preview, targets (checkboxes when `selectable`), the steps
// it would take and the daily caps; a profile check adds the build in a temp
// worktree, the archive check and each target's upload requests. Nothing here is public.
const props = defineProps<{ plan: CheckResult; selectable?: boolean }>()
const selected = defineModel<string[]>('selected', { default: () => [] })
const { t, te } = useI18n()

const refusals = computed(() => props.plan.refusals ?? [])
const targets = computed(() => props.plan.targets ?? [])
const steps = computed(() => props.plan.steps ?? [])
const named = computed(() => targets.value.map((x) => ({ key: x.key, name: x.name, platform: x.platform })))
const writes = computed(() => [...(props.plan.writesVersion ?? []), ...(props.plan.writesChangelog ?? [])])
const headBehind = computed(() => !!props.plan.head && !!props.plan.remoteHead && props.plan.head !== props.plan.remoteHead)

/** A target can be picked: enabled for publishing, configured, uploadable, key accepted. */
function pickable(tg: PlanTarget): boolean {
  return tg.enabled && !targetBlock(tg)
}
function reason(tg: PlanTarget): string {
  return targetBlock(tg) || (!tg.enabled ? t('release.target.off') : '')
}
function toggle(key: string, on: boolean) {
  selected.value = on ? [...new Set([...selected.value, key])] : selected.value.filter((k) => k !== key)
}
const build = computed(() => props.plan.build ?? null)
const archive = computed(() => props.plan.archiveCheck ?? null)
const targetPlans = computed(() => props.plan.targetPlans ?? [])
/** A target dry run's failure: our text for a known code (publish.errors), else the server's. */
function planError(p: CheckTargetPlan): string {
  const k = 'publish.errors.' + (p.code ?? '')
  return p.code && p.code !== 'bad_request' && te(k) ? t(k) : p.error
}
</script>

<template>
  <div class="plan">
    <Message
      v-if="refusals.length"
      severity="warn"
      :closable="false"
    >
      <div class="refusals-title">
        {{ t('release.plan.refused') }}
      </div>
      <ul class="refusals">
        <li
          v-for="r in refusals"
          :key="r.code + r.message"
        >
          {{ refusalText(r) }}
          <span
            v-if="r.message && refusalText(r) !== r.message"
            class="muted detail"
          >— {{ r.message }}</span>
        </li>
      </ul>
    </Message>
    <Message
      v-else-if="plan.ok"
      severity="success"
      :closable="false"
    >
      {{ t('release.plan.ready', { version: plan.version || '—' }) }}
    </Message>

    <dl class="facts">
      <dt>{{ t('release.plan.version') }}</dt>
      <dd class="mono">
        {{ plan.currentVersion || '—' }} → <b>{{ plan.version || '—' }}</b><span
          v-if="plan.tag"
          class="muted"
        > · {{ plan.tag }}</span>
      </dd>
      <template v-if="plan.baseTag">
        <dt>{{ t('release.plan.baseTag') }}</dt>
        <dd class="mono">
          {{ plan.baseTag }}
        </dd>
      </template>
      <template v-if="plan.folder">
        <dt>{{ t('release.plan.folder') }}</dt>
        <dd class="mono path">
          {{ plan.folder }}<span
            v-if="plan.branch"
            class="muted"
          > · {{ plan.branch }}</span>
        </dd>
      </template>
      <template v-if="plan.head">
        <dt>{{ t('release.plan.head') }}</dt>
        <dd class="mono">
          {{ shortSha(plan.head) }}<span
            v-if="headBehind"
            class="warn"
          > ≠ {{ t('release.plan.remote') }} {{ shortSha(plan.remoteHead) }}</span>
        </dd>
      </template>
      <template v-if="plan.asset">
        <dt>{{ t('release.plan.asset') }}</dt>
        <dd class="mono">
          {{ plan.asset }}
        </dd>
      </template>
      <template v-if="writes.length">
        <dt>{{ t('release.plan.writes') }}</dt>
        <dd class="mono">
          {{ writes.join(', ') }}
        </dd>
      </template>
      <dt>{{ t('release.plan.githubRelease') }}</dt>
      <dd>{{ plan.githubRelease ? t('release.yes') : t('release.no') }}</dd>
      <dt>{{ t('release.plan.smoke') }}</dt>
      <dd>
        {{ plan.smokeKind ? smokeKindText(plan.smokeKind) : '—' }}<span
          v-if="!plan.smokeKind || plan.smokeKind === 'none'"
          class="muted"
        > · {{ t('release.plan.smokeNone') }}</span>
      </dd>
      <dt>{{ t('release.plan.caps') }}</dt>
      <dd>
        {{ t('release.plan.capsText', {
          project: plan.caps.projectReleasesToday, projectMax: plan.caps.projectMaxReleases,
          all: plan.caps.releasesToday, allMax: plan.caps.maxReleases,
          pub: plan.caps.publishesToday, pubMax: plan.caps.maxPublishes,
        }) }}
      </dd>
    </dl>

    <section
      v-if="build"
      class="block"
    >
      <div class="block-title">
        <i :class="build.skipped ? 'pi pi-minus-circle muted' : build.ok ? 'pi pi-check-circle ok' : 'pi pi-times-circle bad'" />
        {{ t('release.plan.build') }}
      </div>
      <p
        v-if="build.skipped"
        class="muted hint"
      >
        {{ t('release.check.skipped', { reason: build.skipped }) }}
      </p>
      <dl
        v-else
        class="facts"
      >
        <template v-if="build.head">
          <dt>{{ t('release.plan.head') }}</dt>
          <dd class="mono">
            {{ shortSha(build.head) }}
          </dd>
        </template>
        <template v-if="build.writes?.length">
          <dt>{{ t('release.plan.writes') }}</dt>
          <dd class="mono">
            {{ build.writes.join(', ') }}
          </dd>
        </template>
        <template v-if="build.command">
          <dt>{{ t('release.check.command') }}</dt>
          <dd class="mono path">
            {{ build.command.command }}
            <span class="muted">· {{ build.command.timedOut ? t('release.check.timedOut') : t('release.check.exit', { code: build.command.exitCode }) }}<template v-if="build.command.durationMs"> · {{ elapsed(build.command.durationMs) }}</template></span>
          </dd>
        </template>
        <template v-if="build.artifact">
          <dt>{{ t('release.plan.asset') }}</dt>
          <dd class="mono path">
            {{ build.artifact.name }} · {{ bytes(build.artifact.size) }} · sha256 {{ shortSha(build.artifact.sha256) }}
          </dd>
        </template>
      </dl>
      <div
        v-if="build.error"
        class="bad pre-line"
      >
        {{ build.error }}
      </div>
      <details v-if="build.command?.output">
        <summary class="muted">
          {{ t('release.check.output') }}
        </summary>
        <pre class="pre">{{ build.command.output }}</pre>
      </details>
    </section>

    <section
      v-if="archive"
      class="block"
    >
      <div class="block-title">
        <i :class="archive.skipped ? 'pi pi-minus-circle muted' : archive.ok ? 'pi pi-check-circle ok' : 'pi pi-times-circle bad'" />
        {{ t('release.step.archive_check') }}
      </div>
      <p
        v-if="archive.skipped"
        class="muted hint"
      >
        {{ t('release.check.skipped', { reason: archive.skipped }) }}
      </p>
      <p
        v-if="archive.note"
        class="hint"
      >
        {{ archive.note }}
      </p>
      <div
        v-if="archive.error"
        class="bad pre-line"
      >
        {{ archive.error }}
      </div>
    </section>

    <section
      v-if="targetPlans.length"
      class="block"
    >
      <div class="block-title">
        {{ t('release.check.uploads') }}
      </div>
      <div
        v-for="p in targetPlans"
        :key="p.key"
        class="tplan"
      >
        <div>
          <i :class="p.ok ? 'pi pi-check-circle ok' : 'pi pi-times-circle bad'" />
          {{ targetLabel(p.key, named) }}
        </div>
        <div
          v-if="!p.ok && (p.error || p.code)"
          class="bad"
        >
          {{ planError(p) }}
        </div>
        <ol
          v-if="p.plan?.length"
          class="steps"
        >
          <li
            v-for="(r, i) in p.plan"
            :key="i"
          >
            <span class="mono path">{{ r.method }} {{ r.url }}</span>
            <span
              v-if="r.note"
              class="muted note"
            >{{ r.note }}</span>
            <details v-if="requestText(r.body)">
              <summary class="muted note">
                {{ t('release.details') }}
              </summary>
              <pre class="pre">{{ requestText(r.body) }}</pre>
            </details>
          </li>
        </ol>
      </div>
    </section>

    <section class="block">
      <div class="block-title">
        {{ t('release.plan.targets') }}
      </div>
      <p
        v-if="!targets.length"
        class="muted hint"
      >
        {{ t('release.target.none') }}
      </p>
      <div
        v-for="tg in targets"
        :key="tg.key"
        class="target"
      >
        <Checkbox
          v-if="selectable"
          :model-value="selected.includes(tg.key)"
          binary
          :disabled="!pickable(tg)"
          :input-id="'tg-' + tg.key"
          @update:model-value="(v: boolean) => toggle(tg.key, v)"
        />
        <i
          v-else
          :class="tg.selected ? 'pi pi-check-circle ok' : 'pi pi-minus-circle muted'"
        />
        <PlatformIcon
          :platform="tg.platform"
          :size="14"
        />
        <label
          :for="selectable ? 'tg-' + tg.key : undefined"
          class="target-main"
        >
          <a
            :href="safeUrl(tg.url)"
            target="_blank"
            rel="noopener noreferrer"
            class="name"
            @click.stop
          >{{ tg.name || tg.key }}</a>
          <span
            v-if="tg.latestVersion"
            class="muted mono"
          > · {{ t('release.target.latest', { v: tg.latestVersion }) }}</span>
          <span
            v-if="reason(tg)"
            class="reason"
          >{{ reason(tg) }}</span>
          <span
            v-else-if="tg.auth === 'error' || tg.auth === 'unavailable'"
            class="reason warn"
          >{{ t('release.target.authUnknown') }}<template v-if="tg.error">: {{ tg.error }}</template></span>

        </label>
      </div>
    </section>

    <section
      v-if="plan.changelog"
      class="block"
    >
      <div class="block-title">
        {{ t('release.plan.changelog') }}
      </div>
      <pre class="pre">{{ plan.changelog }}</pre>
    </section>

    <section
      v-if="steps.length"
      class="block"
    >
      <div class="block-title">
        {{ t('release.plan.steps') }}
      </div>
      <ol class="steps">
        <li
          v-for="(s, i) in steps"
          :key="i"
        >
          <span class="step-name">{{ stepLabel(s, named) }}</span>
          <span
            v-if="s.request"
            v-tooltip.top="s.idemKey ? t('release.idemKey', { key: s.idemKey }) : undefined"
            class="mono muted path"
          >{{ s.request }}</span>
          <span
            v-if="s.note"
            class="muted note"
          >{{ noteText(s.note) }}</span>
        </li>
      </ol>
    </section>
  </div>
</template>

<style scoped>
.plan {
  display: flex;
  flex-direction: column;
  gap: 12px;
  font-size: calc(13px * var(--iw-fs, 1));
}

.refusals-title {
  font-weight: 600;
  margin-bottom: 4px;
}

.refusals {
  margin: 0;
  padding-left: 18px;
}

.detail {
  font-size: calc(12px * var(--iw-fs, 1));
}

.facts {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  gap: 4px 14px;
  margin: 0;
}

.facts dt {
  color: var(--iw-muted);
}

.facts dd {
  margin: 0;
  min-width: 0;
}

.path {
  overflow-wrap: anywhere;
}

.warn {
  color: var(--iw-warn);
}

.ok {
  color: var(--iw-success);
}

.bad {
  color: var(--iw-danger);
}

.pre-line {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.tplan {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 4px 0;
}

.block {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.block-title {
  font-weight: 600;
}

.hint {
  margin: 0;
}

.target {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 4px 0;
}

.target > i,
.target > :deep(svg),
.target > :deep(.p-checkbox) {
  margin-top: 2px;
  flex: none;
}

.target-main {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 2px 6px;
  min-width: 0;
}

.target-main .name {
  font-weight: 500;
}

.reason {
  flex-basis: 100%;
  font-size: calc(12px * var(--iw-fs, 1));
  color: var(--iw-muted);
}

.pre {
  margin: 0;
  max-height: 220px;
  overflow: auto;
  padding: 8px 10px;
  border-radius: var(--iw-radius-sm);
  background: var(--iw-elevated);
  font-family: var(--iw-mono, monospace);
  font-size: calc(12px * var(--iw-fs, 1));
  white-space: pre-wrap;
}

.steps {
  margin: 0;
  padding-left: 20px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.steps li {
  display: flex;
  flex-direction: column;
}

.step-name {
  font-weight: 500;
}

.note {
  font-size: calc(12px * var(--iw-fs, 1));
}
</style>
