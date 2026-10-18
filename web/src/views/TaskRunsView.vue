<script setup lang="ts">
/**
 * Probe-run history page for one task. Shows a top "trend" strip (KPIs +
 * sparkline) followed by a paginated list of individual runs.
 *
 * Refreshes every 10s so an admin watching the page can see new ticks land.
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useToast } from '../utils/toast'
import Button from '../components/ui/Button.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import RunTimeline from '../components/runs/RunTimeline.vue'
import RunRow from '../components/runs/RunRow.vue'
import { runsApi, type ProbeRun } from '../api/runs'
import { api } from '../api/client'
import type { Task } from '../api/tasks'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToast()

const task = ref<Task | null>(null)
const runs = ref<ProbeRun[]>([])
const limit = ref(30)
const loading = ref(true)
const refreshing = ref(false)

let timer: number | null = null

const taskId = computed(() => route.params.id as string)

async function fetchAll(silent = false) {
  if (!silent) loading.value = true
  else refreshing.value = true
  try {
    if (!task.value) {
      task.value = await api.get<Task>(`/admin/tasks/${taskId.value}`)
    }
    const { runs: data } = await runsApi.list(taskId.value, limit.value)
    runs.value = data
  } catch (e) {
    const err = e as Error
    if (!silent) {
      toast.error(t('toast.errLoad'), err.message)
    }
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

function startAutoRefresh() {
  if (timer != null) return
  timer = window.setInterval(() => fetchAll(true), 10_000)
}
function stopAutoRefresh() {
  if (timer != null) {
    window.clearInterval(timer)
    timer = null
  }
}

onMounted(async () => {
  await fetchAll()
  startAutoRefresh()
})
onBeforeUnmount(stopAutoRefresh)

function back() {
  router.push(`/tasks/${taskId.value}`)
}

async function reload() {
  await fetchAll()
}
</script>

<template>
  <section class="runs">
    <header class="runs__head">
      <div class="runs__head-left">
        <button type="button" class="runs__back" @click="back">← {{ t('common.back') }}</button>
        <h1>{{ task?.name ?? taskId }}</h1>
        <p class="text-muted text-mono runs__sub">{{ taskId }}</p>
      </div>
      <div class="runs__head-right">
        <span v-if="refreshing" class="runs__pulse text-muted" aria-hidden="true">●</span>
        <Button variant="ghost" @click="reload">{{ t('runs.reload') }}</Button>
      </div>
    </header>

    <div v-if="loading" class="text-muted">{{ t('common.loading') }}</div>
    <template v-else>
      <RunTimeline :runs="runs" />

      <section class="runs__list">
        <header class="runs__list-head">
          <h3>{{ t('runs.list.title') }}</h3>
          <div class="runs__limit">
            <label class="text-muted text-mono" for="run-limit">{{ t('runs.list.limit') }}</label>
            <select id="run-limit" v-model.number="limit" class="runs__select" @change="reload">
              <option :value="10">10</option>
              <option :value="20">20</option>
              <option :value="50">50</option>
              <option :value="100">100</option>
            </select>
          </div>
        </header>

        <EmptyState
          v-if="runs.length === 0"
          :title="t('runs.list.emptyTitle')"
          :description="t('runs.list.emptyDesc')"
          glyph="∿"
        />
        <div v-else class="runs__rows">
          <RunRow v-for="r in runs" :key="r.id" :run="r" />
        </div>
      </section>
    </template>
  </section>
</template>

<style scoped>
.runs { display: flex; flex-direction: column; gap: var(--space-5); }
.runs__head {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  gap: var(--space-4);
}
.runs__head-left { display: flex; flex-direction: column; gap: var(--space-1); min-width: 0; }
.runs__back {
  background: transparent;
  border: 0;
  padding: 0;
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: var(--fs-12);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  cursor: pointer;
  align-self: flex-start;
  transition: color var(--dur-fast) var(--ease-out);
}
.runs__back:hover { color: var(--accent); }
.runs__sub { margin: 0; font-size: var(--fs-12); }
.runs__head-right { display: flex; align-items: center; gap: var(--space-3); }
.runs__pulse {
  font-size: var(--fs-12);
  color: var(--accent);
  animation: pulse 1.6s var(--ease-out) infinite;
}
@keyframes pulse {
  0%, 100% { opacity: 0.3; }
  50%      { opacity: 1; }
}
@media (prefers-reduced-motion: reduce) {
  .runs__pulse { animation: none; opacity: 0.6; }
}

.runs__list { display: flex; flex-direction: column; gap: var(--space-3); }
.runs__list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  padding: 0 var(--space-1);
}
.runs__list-head h3 {
  margin: 0;
  font-size: var(--fs-13);
  color: var(--muted);
  font-family: var(--font-mono);
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.runs__limit { display: flex; align-items: center; gap: var(--space-2); font-size: var(--fs-11); letter-spacing: 0.06em; text-transform: uppercase; }
.runs__select {
  background: var(--surface);
  color: var(--text);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-sm);
  padding: 2px var(--space-2);
  font-family: var(--font-mono);
  font-size: var(--fs-12);
  cursor: pointer;
}
.runs__select:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }

.runs__rows { display: flex; flex-direction: column; gap: var(--space-2); }
</style>