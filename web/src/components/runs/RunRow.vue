<script setup lang="ts">
/**
 * A single probe-run row. Reads dense, reads mono, reads left-to-right:
 *   status glyph + relative time | TTFT | ITL | chunks | duration | error chip
 *
 * Status uses an ASCII glyph in addition to color (color-blind safe).
 * Errors are truncated to 96 chars by default; click the chip to expand
 * the full body inline so we never lose context.
 *
 * §18.13 — adds an optional "wire capture" section: X-Req-Id, request
 * body, bounded response head + tail (HAR-style). Sections collapse by
 * default to keep the list scannable.
 *
 * §18.14 — the X-Req-Id is promoted to an always-visible chip on every
 * row (it is the handle admins need to grep upstream logs), and a
 * "content" section shows the parsed input / thinking / output.
 */
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ProbeRun, RunStatus } from '../../api/runs'

const props = defineProps<{ run: ProbeRun }>()
const { t, locale } = useI18n()

const expanded = ref(false)
const wireOpen = ref(false)
const contentOpen = ref(false)
const overflows = computed(() => (props.run.error?.length ?? 0) > 96)
const visibleError = computed(() => {
  const e = props.run.error ?? ''
  if (expanded.value || !overflows.value) return e
  return e.slice(0, 96) + '…'
})

const hasCapture = computed(
  () =>
    !!props.run.request_id ||
    !!props.run.request_body ||
    !!props.run.response_head ||
    !!props.run.response_tail,
)

// §18.14 — semantic content. Legacy rows have none of these fields, so the
// section stays hidden for them.
const hasContent = computed(
  () =>
    !!props.run.input_content ||
    !!props.run.thinking_content ||
    !!props.run.output_content,
)

// §18.14 — a one-line size hint so the collapsed row still communicates
// what is inside without opening the section.
const contentSummary = computed(() => {
  const n = (s?: string) => (s?.length ?? 0)
  const parts: string[] = []
  if (n(props.run.input_content)) parts.push(`${t('runs.row.content.input')} ${n(props.run.input_content)}`)
  if (n(props.run.thinking_content)) parts.push(`${t('runs.row.content.thinking')} ${n(props.run.thinking_content)}`)
  if (n(props.run.output_content)) parts.push(`${t('runs.row.content.output')} ${n(props.run.output_content)}`)
  return parts.join(' · ')
})

interface Glyph {
  glyph: string
  tone: 'ok' | 'danger' | 'warn'
  label: string
}
const GLYPHS: Record<RunStatus, Glyph> = {
  success: { glyph: '✓', tone: 'ok', label: 'success' },
  failure: { glyph: '✕', tone: 'danger', label: 'failure' },
  unauthorized: { glyph: '⊘', tone: 'warn', label: 'unauthorized' },
}
const meta = computed(() => GLYPHS[props.run.status])

const startedAt = computed(() => {
  const d = new Date(props.run.started_at)
  return d.toLocaleString(locale.value === 'zh-CN' ? 'zh-CN' : 'en-US', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
})

function copy(text: string) {
  navigator.clipboard?.writeText(text).catch(() => {})
}
</script>

<template>
  <article class="run" :data-status="run.status" :data-wire-open="wireOpen">
    <header class="run__head">
      <span class="run__glyph" :data-tone="meta.tone" :title="t(`runs.status.${meta.label}`)">{{ meta.glyph }}</span>
      <time class="run__time text-mono" :datetime="run.started_at">{{ startedAt }}</time>
      <span class="run__sep" aria-hidden="true">/</span>
      <span class="run__id text-mono text-muted" :title="`#${run.id}`">#{{ run.id }}</span>
    </header>

    <dl class="run__metrics">
      <div class="run__metric">
        <dt>TTFT</dt>
        <dd class="text-mono">{{ run.ttft_ms }}<small>ms</small></dd>
      </div>
      <div class="run__metric">
        <dt>{{ t('runs.row.itl') }}</dt>
        <dd class="text-mono">{{ run.itl_avg_ms }}<small>ms</small></dd>
      </div>
      <div class="run__metric">
        <dt>{{ t('runs.row.chunks') }}</dt>
        <dd class="text-mono">{{ run.chunks }}</dd>
      </div>
      <div class="run__metric run__metric--lead">
        <dt>{{ t('runs.row.duration') }}</dt>
        <dd class="text-mono">{{ run.duration_ms }}<small>ms</small></dd>
      </div>
      <!--
        §18.14 — the X-Req-Id is always visible, not buried in a collapsed
        section: it is the one handle an admin needs to correlate this probe
        with the upstream provider's own logs. Truncated to 8 chars + a
        copy button (the full value is in the title attribute). It lives in
        the metrics grid (not the 220px head column) so it can never
        overflow into the metrics.
      -->
      <div
        v-if="run.request_id"
        class="run__reqid text-mono"
        :title="`${t('runs.row.reqId')}: ${run.request_id}`"
      >
        <span class="run__reqid-label">{{ t('runs.row.reqId') }}</span>
        <span class="run__reqid-value">{{ run.request_id.slice(0, 8) }}</span>
        <button
          type="button"
          class="run__copy run__copy--inline"
          :title="t('runs.row.copyReqId')"
          @click="copy(run.request_id!)"
        >⌘C</button>
      </div>
    </dl>

    <p v-if="run.error" class="run__error text-mono" :data-tone="meta.tone">
      <span class="run__error-prefix">{{ t('runs.row.errPrefix') }}</span>
      <button
        v-if="overflows"
        type="button"
        class="run__error-body run__error-body--toggle"
        :aria-expanded="expanded"
        @click="expanded = !expanded"
      >{{ visibleError }}</button>
      <span v-else class="run__error-body">{{ visibleError }}</span>
      <button
        v-if="run.error"
        type="button"
        class="run__copy"
        :title="t('runs.row.copyErr')"
        @click="copy(run.error!)"
      >⌘C</button>
    </p>
    <p v-else class="run__error run__error--none text-muted">{{ t('runs.row.errNone') }}</p>

    <!--
      §18.14 — the upstream's own error body. On 401/403/5xx this is
      usually where the actionable detail lives ("Incorrect API key
      provided"), so it is surfaced right under the error line.
    -->
    <div v-if="run.response_error_body" class="run__errbody">
      <span class="run__errbody-label">{{ t('runs.row.errBody') }}</span>
      <pre class="run__errbody-pre text-mono">{{ run.response_error_body }}</pre>
      <button
        type="button"
        class="run__copy"
        :title="t('runs.row.copyErrBody')"
        @click="copy(run.response_error_body!)"
      >⌘C</button>
    </div>

    <!--
      §18.14 — semantic content: what we sent, what the model thought,
      what it answered. This is the human-readable counterpart to the
      raw wire capture below.
    -->
    <button
      v-if="hasContent"
      type="button"
      class="run__content-toggle run__wire-toggle text-mono"
      :aria-expanded="contentOpen"
      @click="contentOpen = !contentOpen"
    >
      <span aria-hidden="true">{{ contentOpen ? '▾' : '▸' }}</span>
      <span>{{ t('runs.row.content.title') }}</span>
      <span class="run__wire-id text-muted">{{ contentSummary }}</span>
    </button>
    <section v-if="hasContent && contentOpen" class="run__content">
      <div class="run__wire-row">
        <dt class="run__wire-label">{{ t('runs.row.content.input') }}</dt>
        <dd class="run__wire-value">
          <pre v-if="run.input_content" class="run__wire-pre">{{ run.input_content }}</pre>
          <span v-else class="text-muted">{{ t('runs.row.content.missing') }}</span>
        </dd>
        <button
          v-if="run.input_content"
          type="button"
          class="run__copy"
          :title="t('runs.row.content.copyInput')"
          @click="copy(run.input_content!)"
        >⌘C</button>
      </div>
      <div class="run__wire-row">
        <dt class="run__wire-label">{{ t('runs.row.content.thinking') }}</dt>
        <dd class="run__wire-value">
          <pre v-if="run.thinking_content" class="run__wire-pre run__wire-pre--thinking">{{ run.thinking_content }}</pre>
          <span v-else class="text-muted">{{ t('runs.row.content.noThinking') }}</span>
        </dd>
        <button
          v-if="run.thinking_content"
          type="button"
          class="run__copy"
          :title="t('runs.row.content.copyThinking')"
          @click="copy(run.thinking_content!)"
        >⌘C</button>
      </div>
      <div class="run__wire-row">
        <dt class="run__wire-label">{{ t('runs.row.content.output') }}</dt>
        <dd class="run__wire-value">
          <pre v-if="run.output_content" class="run__wire-pre run__wire-pre--output">{{ run.output_content }}</pre>
          <span v-else class="text-muted">{{ t('runs.row.content.missing') }}</span>
        </dd>
        <button
          v-if="run.output_content"
          type="button"
          class="run__copy"
          :title="t('runs.row.content.copyOutput')"
          @click="copy(run.output_content!)"
        >⌘C</button>
      </div>
      <p v-if="run.content_truncated" class="run__wire-note">
        {{ t('runs.row.content.truncatedNote') }}
      </p>
    </section>

    <!-- §18.13 wire capture. Hidden when no fields were persisted (legacy rows). -->
    <button
      v-if="hasCapture"
      type="button"
      class="run__wire-toggle text-mono"
      :aria-expanded="wireOpen"
      @click="wireOpen = !wireOpen"
    >
      <span aria-hidden="true">{{ wireOpen ? '▾' : '▸' }}</span>
      <span>{{ t('runs.row.wire.title') }}</span>
      <span v-if="run.request_id" class="run__wire-id text-muted">{{ run.request_id.slice(0, 8) }}…</span>
    </button>
    <section v-if="hasCapture && wireOpen" class="run__wire">
      <div class="run__wire-row">
        <dt class="run__wire-label">{{ t('runs.row.wire.requestId') }}</dt>
        <dd class="run__wire-value text-mono">{{ run.request_id || t('runs.row.wire.missing') }}</dd>
        <button
          v-if="run.request_id"
          type="button"
          class="run__copy"
          :title="t('runs.row.wire.requestId')"
          @click="copy(run.request_id!)"
        >⌘C</button>
      </div>
      <div class="run__wire-row">
        <dt class="run__wire-label">{{ t('runs.row.wire.request') }}</dt>
        <dd class="run__wire-value">
          <pre v-if="run.request_body" class="run__wire-pre">{{ run.request_body }}</pre>
          <span v-else class="text-muted">{{ t('runs.row.wire.missing') }}</span>
        </dd>
        <button
          v-if="run.request_body"
          type="button"
          class="run__copy"
          :title="t('runs.row.wire.copyReq')"
          @click="copy(run.request_body!)"
        >⌘C</button>
        <p v-if="run.request_truncated" class="run__wire-note">
          {{ t('runs.row.wire.requestTruncatedNote') }}
        </p>
      </div>
      <div class="run__wire-row">
        <dt class="run__wire-label">{{ t('runs.row.wire.responseHead') }}</dt>
        <dd class="run__wire-value">
          <pre v-if="run.response_head" class="run__wire-pre">{{ run.response_head }}</pre>
          <span v-else class="text-muted">{{ t('runs.row.wire.missing') }}</span>
        </dd>
        <button
          v-if="run.response_head"
          type="button"
          class="run__copy"
          :title="t('runs.row.wire.copyRespHead')"
          @click="copy(run.response_head!)"
        >⌘C</button>
      </div>
      <div class="run__wire-row">
        <dt class="run__wire-label">{{ t('runs.row.wire.responseTail') }}</dt>
        <dd class="run__wire-value">
          <pre v-if="run.response_tail" class="run__wire-pre">{{ run.response_tail }}</pre>
          <span v-else class="text-muted">{{ t('runs.row.wire.missing') }}</span>
        </dd>
        <button
          v-if="run.response_tail"
          type="button"
          class="run__copy"
          :title="t('runs.row.wire.copyRespTail')"
          @click="copy(run.response_tail!)"
        >⌘C</button>
        <p v-if="run.response_truncated" class="run__wire-note">
          {{ t('runs.row.wire.responseTruncatedNote') }}
        </p>
      </div>
    </section>
  </article>
</template>

<style scoped>
.run {
  display: grid;
  grid-template-columns: 220px 1fr;
  grid-template-areas:
    'head metrics'
    'error error'
    'errbody errbody'
    'content-toggle content-toggle'
    'content content'
    'wire-toggle wire-toggle'
    'wire wire'
    'error2 error2';
  gap: var(--space-3) var(--space-5);
  padding: var(--space-4) var(--space-5);
  border: 1px solid var(--border);
  border-left: 2px solid var(--border-strong);
  border-radius: var(--radius-md);
  background: var(--surface);
  transition: background var(--dur-fast) var(--ease-out), border-color var(--dur-fast) var(--ease-out);
}
.run:hover { background: var(--surface-2); }
.run[data-status='success']     { border-left-color: var(--ok); }
.run[data-status='failure']     { border-left-color: var(--danger); }
.run[data-status='unauthorized']{ border-left-color: var(--warn); }

.run__head {
  grid-area: head;
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
}
.run__glyph {
  font-family: var(--font-mono);
  font-size: var(--fs-16);
  width: 1.4em;
  text-align: center;
  line-height: 1;
}
.run__glyph[data-tone='ok']     { color: var(--ok); }
.run__glyph[data-tone='danger'] { color: var(--danger); }
.run__glyph[data-tone='warn']   { color: var(--warn); }
.run__time { font-size: var(--fs-13); }
.run__sep  { color: var(--muted-2); }
.run__id   { font-size: var(--fs-11); }

/* §18.14 — always-visible X-Req-Id chip. It is the trailing grid item of
   the metrics row (right edge), so it participates in the layout instead
   of overflowing the 220px head column. */
.run__reqid {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  justify-self: end;
  align-self: start;
  padding: 1px var(--space-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-size: var(--fs-11);
  color: var(--muted);
  white-space: nowrap;
}
.run__reqid-label {
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--muted-2);
}
.run__reqid-value {
  color: var(--text);
  letter-spacing: 0.04em;
}
.run__copy--inline {
  padding: 0 var(--space-1);
  font-size: var(--fs-11);
  line-height: 1.4;
}

.run__metrics {
  grid-area: metrics;
  display: grid;
  /* 4 metrics + the X-Req-Id chip as an auto-sized trailing column. */
  grid-template-columns: repeat(4, minmax(0, 1fr)) auto;
  gap: var(--space-4);
  margin: 0;
}
.run__metric dt {
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--muted);
  margin-bottom: 2px;
}
.run__metric dd {
  margin: 0;
  font-size: var(--fs-14);
  color: var(--text);
}
.run__metric dd small {
  font-size: 0.7em;
  color: var(--muted);
  margin-left: 2px;
}
.run__metric--lead dd { font-weight: var(--fw-semibold); color: var(--text); }

.run__error {
  grid-area: error;
  margin: 0;
  padding: var(--space-2) var(--space-3);
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-size: var(--fs-12);
  white-space: pre-wrap;
  word-break: break-word;
  display: flex;
  align-items: flex-start;
  gap: var(--space-2);
}
.run__error-prefix {
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  flex-shrink: 0;
  padding-top: 2px;
}
.run__error-body { flex: 1; min-width: 0; }
.run__error-body--toggle {
  background: transparent;
  border: 0;
  padding: 0;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.run__error-body--toggle:hover { color: var(--accent); }
.run__error[data-tone='failure']      { border-left: 2px solid var(--danger); }
.run__error[data-tone='unauthorized'] { border-left: 2px solid var(--warn); }
.run__error--none {
  font-style: italic;
  background: transparent;
  border: 1px dashed var(--border);
}
.run__copy {
  background: transparent;
  border: 1px solid var(--border);
  color: var(--muted);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  padding: 2px var(--space-2);
  cursor: pointer;
  transition: color var(--dur-fast) var(--ease-out), border-color var(--dur-fast) var(--ease-out);
  flex-shrink: 0;
}
.run__copy:hover { color: var(--accent); border-color: var(--accent); }

/* §18.13 wire capture. */
.run__wire-toggle {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  background: transparent;
  border: 1px dashed var(--border);
  color: var(--muted);
  border-radius: var(--radius-sm);
  padding: var(--space-1) var(--space-3);
  font-size: var(--fs-12);
  letter-spacing: 0.04em;
  text-transform: uppercase;
  cursor: pointer;
  transition: color var(--dur-fast) var(--ease-out), border-color var(--dur-fast) var(--ease-out);
  align-self: flex-start;
  justify-self: flex-start;
}
/* §18.14 — the toggle is shared by two sections, so grid placement is
   assigned explicitly rather than by whichever rule happens to come last. */
.run__content-toggle { grid-area: content-toggle; }
.run__wire-toggle:not(.run__content-toggle) { grid-area: wire-toggle; }
.run__wire-toggle:hover { color: var(--accent); border-color: var(--accent); }
.run__wire-id { text-transform: none; letter-spacing: 0; }

.run__wire {
  grid-area: wire;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-3);
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}
.run__wire-row {
  display: grid;
  grid-template-columns: 100px 1fr auto;
  gap: var(--space-3);
  align-items: flex-start;
}
.run__wire-label {
  margin: 0;
  padding-top: 6px;
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.run__wire-value { margin: 0; min-width: 0; }
.run__wire-pre {
  margin: 0;
  padding: var(--space-2);
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-size: var(--fs-12);
  line-height: 1.5;
  max-height: 280px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.run__wire-note {
  grid-column: 2 / -1;
  margin: var(--space-1) 0 0;
  padding: var(--space-1) var(--space-2);
  background: var(--accent-soft);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
}

/* §18.14 — semantic content section. */
.run__content {
  grid-area: content;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-3);
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

/* §18.14 — thinking vs answer get a subtle left edge so the two
   narrative streams are distinguishable at a glance. */
.run__wire-pre--thinking {
  border-left: 2px solid var(--warn);
}
.run__wire-pre--output {
  border-left: 2px solid var(--ok);
}

/* §18.14 — upstream error body. */
.run__errbody {
  grid-area: errbody;
  display: grid;
  grid-template-columns: 100px 1fr auto;
  gap: var(--space-3);
  align-items: start;
  padding: var(--space-2) var(--space-3);
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-left: 2px solid var(--danger);
  border-radius: var(--radius-sm);
}
.run__errbody-label {
  padding-top: 4px;
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.run__errbody-pre {
  margin: 0;
  max-height: 160px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text);
  font-size: var(--fs-12);
  line-height: 1.5;
}

@media (max-width: 880px) {
  .run {
    grid-template-columns: 1fr;
    grid-template-areas:
      'head'
      'metrics'
      'error'
      'errbody'
      'content-toggle'
      'content'
      'wire-toggle'
      'wire';
  }
  .run__metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .run__wire-row { grid-template-columns: 1fr; }
  .run__errbody { grid-template-columns: 1fr; }
  /* The chip flows as a normal 2-col grid item on narrow screens. */
  .run__reqid { justify-self: start; }
}
</style>
