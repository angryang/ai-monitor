/**
 * AI request manager store. Holds the flat list of requests, the
 * currently running batch, and the selection the operator is building up.
 */

import { defineStore } from 'pinia'
import { useProvidersStore } from './providers'
import {
  requestsApi,
  type AIRequest,
  type AIRequestWritePayload,
  type RequestBatch,
  type RequestContentOverrides,
} from '../api/requests'

interface State {
  requests: AIRequest[]
  loading: boolean
  lastError: string | null
  /** The batch the result panel is showing. */
  batch: RequestBatch | null
  /** Id of the batch whose poll loop owns the panel; older loops stop updating. */
  activeBatchId: string | null
  /** True while a batch is being polled. */
  polling: boolean
  /** Request ids the operator has selected. */
  selected: string[]
}

/**
 * The server marshals empty slices as [] today, but a proxy or an older
 * binary can hand back null. Keep every downstream .length safe.
 */
function normalizeBatch(b: RequestBatch): RequestBatch {
  return { ...b, skipped: b.skipped ?? [], results: b.results ?? [] }
}

export const useRequestsStore = defineStore('requests', {
  state: (): State => ({
    requests: [],
    loading: false,
    lastError: null,
    batch: null,
    activeBatchId: null,
    polling: false,
    selected: [],
  }),
  getters: {
    /** Requests whose provider is healthy enough to actually be fired. */
    runnableSelected(state): string[] {
      const providers = useProvidersStore()
      const broken = new Set(
        providers.providers
          .filter((p) => !!p.secret_error || !p.has_api_key || !p.enabled)
          .map((p) => p.id),
      )
      return state.selected.filter((id) => {
        const req = state.requests.find((r) => r.id === id)
        return !!req && !broken.has(req.provider_id)
      })
    },
  },
  actions: {
    async fetchAll() {
      this.loading = true
      this.lastError = null
      try {
        const { requests } = await requestsApi.list()
        this.requests = requests ?? []
        // Drop selections that no longer exist (deleted in another tab).
        const alive = new Set(this.requests.map((r) => r.id))
        this.selected = this.selected.filter((id) => alive.has(id))
      } catch (e) {
        this.lastError = (e as Error).message
      } finally {
        this.loading = false
      }
    },

    async create(payload: AIRequestWritePayload): Promise<AIRequest> {
      const r = await requestsApi.create(payload)
      this.requests = [...this.requests, r]
      return r
    },

    async update(id: string, payload: Partial<AIRequestWritePayload>): Promise<AIRequest> {
      const r = await requestsApi.update(id, payload)
      this.requests = this.requests.map((x) => (x.id === id ? r : x))
      return r
    },

    async remove(id: string) {
      await requestsApi.remove(id)
      this.requests = this.requests.filter((x) => x.id !== id)
      this.selected = this.selected.filter((sid) => sid !== id)
    },

    toggleSelection(id: string) {
      const idx = this.selected.indexOf(id)
      if (idx >= 0) this.selected.splice(idx, 1)
      else this.selected.push(id)
    },

    selectAll() {
      const ids = this.requests.map((r) => r.id)
      const allSelected = ids.every((id) => this.selected.includes(id))
      this.selected = allSelected
        ? this.selected.filter((id) => !ids.includes(id))
        : [...new Set([...this.selected, ...ids])]
    },

    clearSelection() {
      this.selected = []
    },

    /** Start showing a batch and hand its id to the poll loop. */
    startBatch(batch: RequestBatch): RequestBatch {
      const safe = normalizeBatch(batch)
      this.batch = safe
      this.activeBatchId = safe.batch_id
      this.polling = safe.state === 'running'
      if (this.polling) void this.poll(safe.batch_id)
      return safe
    },

    /**
     * Fire the current selection and start polling for the results.
     * `overrides` is one-shot content typed in the run dialog; it never
     * changes the saved requests.
     */
    async runSelected(overrides?: RequestContentOverrides): Promise<RequestBatch> {
      return this.runIds(this.runnableSelected, overrides)
    },

    /** Fire an explicit set of ids — used when the operator runs one row. */
    async runIds(ids: string[], overrides?: RequestContentOverrides): Promise<RequestBatch> {
      return this.startBatch(await requestsApi.run(ids, { overrides }))
    },

    /**
     * Poll a batch until it finishes. The server answers 202 immediately
     * and keeps the batch in memory; the requests themselves may take
     * tens of seconds, so the UI polls rather than holding the
     * connection open.
     */
    async poll(batchId: string): Promise<void> {
      try {
        const batch = normalizeBatch(await requestsApi.getBatch(batchId))
        // A newer batch may have taken over the panel while this poll was
        // in flight; its loop must not be overwritten by a stale response.
        if (this.activeBatchId !== batchId) return
        this.batch = batch
        if (batch.state === 'running') {
          window.setTimeout(() => {
            void this.poll(batchId)
          }, 700)
          return
        }
        this.polling = false
      } catch (e) {
        if (this.activeBatchId !== batchId) return
        this.polling = false
        this.lastError = (e as Error).message
      }
    },
  },
})
