<script lang="ts">
import { defineComponent, h, type VNode } from 'vue'
import { markdown, type Inline } from '../lib/markdown'
import { safeUrl } from '../lib/safeUrl'

// Renders lib/markdown tokens as text nodes and fixed elements (no v-html), inside a pre-wrap body.
function inl(s: Inline): VNode | string {
  if (s.t === 'code') return h('code', { class: 'md-code' }, s.v)
  if (s.t === 'b') return h('strong', s.v)
  if (s.t === 'i') return h('em', s.v)
  if (s.t === 'a') {
    const href = safeUrl(s.href)
    return href ? h('a', { href, target: '_blank', rel: 'noopener noreferrer' }, s.v) : s.v
  }
  return s.v
}

export default defineComponent({
  props: { text: { type: String, required: true } },
  setup(props) {
    return () => markdown(props.text).map((b) => (b.t === 'pre' ? h('pre', { class: 'md-pre' }, h('code', b.v)) : h('div', b.v.map(inl))))
  },
})
</script>

<style scoped>
.md-code,
.md-pre {
  font-family: var(--iw-mono);
  font-size: 0.9em;
  background: var(--iw-elevated);
  border-radius: 6px;
}

.md-code {
  padding: 1px 5px;
}

.md-pre {
  margin: 8px 0;
  padding: 10px 12px;
  overflow-x: auto;
  white-space: pre;
  line-height: 1.5;
}
</style>
