/**
 * AI request manager API surface.
 *
 * A request is one predefined prompt bound to one AI service provider.
 * There is no request-group layer: every request points directly at its
 * provider, so different requests can target different upstreams in the
 * same list and in the same batch.
 *
 * A provider offers a set of candidate models; this request selects exactly
 * one of them. The server nests a read-only provider summary (with its
 * candidate `models`) in every request so the list can be rendered in one
 * round trip. The provider api key never leaves the server — only
 * `has_api_key`.
 */

import { api } from './client'
import type { Provider } from './providers'

export type BatchState = 'running' | 'done'

export type BatchItemStatus = 'success' | 'failure' | 'unauthorized'

/** One predefined AI request. */
export interface AIRequest {
  id: string
  /** The AI service provider this request is sent to. */
  provider_id: string
  /** 服务商提供候选模型，本实体选定其中一个。 */
  model: string
  name: string
  description: string
  system_prompt: string
  user_prompt: string
  prompt_vars: Record<string, string>
  order: number
  /** null / undefined means "inherit the provider's thinking switch". */
  thinking_override?: boolean | null
  /** Read-only summary of the upstream this request points at. */
  provider?: Provider | null
  created_at: string
  updated_at: string
}

export interface AIRequestWritePayload {
  name: string
  description?: string
  /** The provider to send this request to. Required on create. */
  provider_id: string
  /** 服务商提供候选模型，本实体选定其中一个。必须是所选 provider 的 models 之一。 */
  model: string
  system_prompt?: string
  user_prompt: string
  prompt_vars?: Record<string, string>
  order?: number
  /** Omit on PATCH to keep the stored value; false disables thinking. */
  thinking_override?: boolean | null
}

/** A request that was never sent, with the reason why. */
export interface BatchSkipped {
  id: string
  name: string
  reason: string
}

/** The outcome of one request inside a batch. */
export interface BatchItemResult {
  request_id: string
  request_name: string
  /** Which upstream this particular request was sent to. */
  provider_id: string
  provider_name: string
  protocol: string
  model: string
  status: BatchItemStatus
  error?: string
  duration_ms: number
  ttft_ms: number
  chunks: number
  /** X-Req-Id echoed back by the upstream, for cross-referencing its logs. */
  x_request_id?: string
  input?: string
  thinking?: string
  output?: string
  started_at: string
  ended_at: string
}

/**
 * Ad-hoc content for one request, scoped to a single execution. Omitted
 * fields keep the saved predefined content; `prompt_vars: {}` clears every
 * variable for this run. Nothing here is persisted.
 */
export interface RequestContentOverride {
  system_prompt?: string
  user_prompt?: string
  prompt_vars?: Record<string, string>
}

/** Custom content keyed by request id for one batch. */
export type RequestContentOverrides = Record<string, RequestContentOverride>

export interface RunRequestOptions {
  concurrency?: number
  overrides?: RequestContentOverrides
}

/** One execution of one or more requests. */
export interface RequestBatch {
  batch_id: string
  state: BatchState
  total: number
  completed: number
  succeeded: number
  failed: number
  skipped: BatchSkipped[]
  results: BatchItemResult[]
  started_at: string
  ended_at: string
}

export const requestsApi = {
  /** Flat list of every request, each with its provider nested. */
  list() {
    return api.get<{ requests: AIRequest[] }>('/admin/requests')
  },
  get(id: string) {
    return api.get<AIRequest>(`/admin/requests/${id}`)
  },
  create(payload: AIRequestWritePayload) {
    return api.post<AIRequest>('/admin/requests', payload)
  },
  update(id: string, payload: Partial<AIRequestWritePayload>) {
    return api.patch<AIRequest>(`/admin/requests/${id}`, payload)
  },
  remove(id: string) {
    return api.delete<{ ok: boolean }>(`/admin/requests/${id}`)
  },
  /**
   * Fire a selection of requests — one or many, possibly across several
   * providers. `overrides` carries the per-request content typed for this
   * run only; the saved requests stay untouched.
   */
  run(requestIds: string[], options: RunRequestOptions = {}) {
    return api.post<RequestBatch>('/admin/requests/run', {
      request_ids: requestIds,
      concurrency: options.concurrency,
      overrides: options.overrides,
    })
  },
  /** Poll the live progress of a batch. */
  getBatch(id: string) {
    return api.get<RequestBatch>(`/admin/request-batches/${id}`)
  },
}
