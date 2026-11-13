/**
 * Task API surface.
 *
 * A monitoring task names the AI service provider it probes and owns only
 * what makes it a monitoring task: the prompt, the schedule, the canary
 * group and the notification switch. The upstream coordinates (protocol,
 * url, credentials, timeout, TLS / thinking switches) live on the provider.
 * A provider offers a set of candidate models; this task selects exactly one
 * of them. The provider (with its candidate `models`) is nested in the
 * response so the list renders in one round trip.
 *
 * Note the response model NEVER includes api_key or webhook values — the
 * server uses `has_api_key` / `has_webhook` booleans so we can render the
 * form without leaking the ciphertext.
 */

import { api } from './client'
import type { Provider } from './providers'

export interface TaskNotify {
  enabled: boolean
  channel: string
  has_webhook: boolean
}

export interface Task {
  id: string
  name: string
  enabled: boolean
  /** The AI service provider this task probes. */
  provider_id: string
  /** 服务商提供候选模型，本实体选定其中一个。 */
  model: string
  /** The provider, nested so the list renders without a second round trip. */
  provider?: Provider | null
  /** Monitoring tasks only send a user prompt; system prompts belong to AI requests. */
  user_prompt: string
  prompt_vars: Record<string, string>
  schedule: string
  canary_group: string
  notify: TaskNotify
  /** Whether a notification webhook is configured. */
  has_webhook: boolean
  /**
   * Non-empty when the server could not decrypt this task's stored secrets
   * with the current master key. The task is listed but has no usable
   * notification channel until the webhook is re-entered.
   */
  secret_error?: string
  created_at: string
  updated_at: string
}

export interface TaskWritePayload {
  name: string
  enabled?: boolean
  /** The AI service provider this task probes. */
  provider_id: string
  /** 服务商提供候选模型，本实体选定其中一个。必须是所选 provider 的 models 之一。 */
  model: string
  user_prompt: string
  prompt_vars?: Record<string, string>
  schedule: string
  canary_group?: string
  notify: {
    enabled: boolean
    channel?: string
    wecom_webhook?: string
  }
}

export const tasksApi = {
  list() {
    return api.get<{ tasks: Task[] }>('/admin/tasks')
  },
  get(id: string) {
    return api.get<Task>(`/admin/tasks/${id}`)
  },
  create(payload: TaskWritePayload) {
    return api.post<Task>('/admin/tasks', payload)
  },
  update(id: string, payload: Partial<TaskWritePayload>) {
    return api.patch<Task>(`/admin/tasks/${id}`, payload)
  },
  remove(id: string) {
    return api.delete<{ ok: boolean }>(`/admin/tasks/${id}`)
  },
  start(id: string) {
    return api.post<Task>(`/admin/tasks/${id}/start`, {})
  },
  stop(id: string) {
    return api.post<Task>(`/admin/tasks/${id}/stop`, {})
  },
}
