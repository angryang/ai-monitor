/**
 * ProbeRun API surface — one row per executed probe. Read-only on the
 * client side; the server owns persistence via RunStore.
 */

import { api, apiFetch } from './client'

export type RunStatus = 'success' | 'failure' | 'unauthorized'

export interface ProbeRun {
  id: number
  task_id: string
  task_name: string
  protocol: string
  model: string
  status: RunStatus
  /** Truncated to 4096 chars server-side; may be empty. */
  error?: string
  started_at: string
  ended_at: string
  duration_ms: number
  ttft_ms: number
  itl_avg_ms: number
  itl_count: number
  chunks: number
  // §18.13 — wire capture. May all be empty for legacy rows.
  /** X-Req-Id echoed back from the adapter (default = uuid v4). */
  request_id?: string
  /** JSON body POSTed to the upstream. Default cap = 32 KiB. */
  request_body?: string
  /** First N bytes of the response stream (default N = 4 KiB). */
  response_head?: string
  /** Last N bytes of the response stream (default N = 4 KiB). */
  response_tail?: string
  /** True when the persisted request body is shorter than what was sent. */
  request_truncated?: boolean
  /** True when response middle bytes were dropped. */
  response_truncated?: boolean
  // §18.14 — semantic content. Each field is capped server-side
  // (16 KiB) and may be empty for legacy rows or models that do not
  // emit reasoning.
  /** Input actually sent to the model (system + user, vars expanded). */
  input_content?: string
  /** Model's reasoning trace; empty when the model emits none. */
  thinking_content?: string
  /** The model's answer text. */
  output_content?: string
  /** True when any of the three content fields hit the size cap. */
  content_truncated?: boolean
  /** Body of a failed (4xx/5xx) response; empty on success. */
  response_error_body?: string
}

export const runsApi = {
  /** Fetch the most-recent runs for a task. Newest first. */
  list(taskId: string, limit = 20) {
    return apiFetch<{ runs: ProbeRun[] }>(`/admin/tasks/${taskId}/runs`, {
      method: 'GET',
      query: { limit },
    })
  },
}