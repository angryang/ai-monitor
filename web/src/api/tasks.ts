/**
 * Task API surface. Note the response model NEVER includes api_key or
 * webhook values — the server uses `has_api_key` / `has_webhook` booleans
 * so we can render the form without leaking the ciphertext.
 */

import { api } from './client'

export type Protocol = 'openai' | 'anthropic' | 'gemini'

export interface TaskNotify {
  enabled: boolean
  channel: string
  has_webhook: boolean
}

export interface Task {
  id: string
  name: string
  enabled: boolean
  protocol: Protocol
  base_url: string
  model: string
  system_prompt: string
  user_prompt: string
  prompt_vars: Record<string, string>
  schedule: string
  timeout: string
  canary_group: string
  /** Per-task switch: skip upstream TLS certificate verification. */
  insecure_skip_verify: boolean
  /** Instance-wide switch: when true every probe skips verification. */
  insecure_skip_verify_global: boolean
  /**
   * Per-task switch: ask the upstream model to emit its reasoning trace.
   * Off means the probe sends no thinking parameter and follows the
   * upstream default.
   */
  thinking_enabled: boolean
  notify: TaskNotify
  has_api_key: boolean
  /**
   * Non-empty when the server could not decrypt this task's stored secrets
   * with the current master key. The task is listed but has no usable
   * credentials until the api key is re-entered.
   */
  secret_error?: string
  created_at: string
  updated_at: string
  openai?: { stream_path?: string; extra_headers?: string } | null
  anthropic?: { max_tokens?: number; extra_headers?: string } | null
  gemini?: { alt?: string; extra_headers?: string } | null
}

export interface TaskWritePayload {
  name: string
  enabled?: boolean
  protocol: Protocol
  base_url: string
  api_key?: string
  model: string
  system_prompt?: string
  user_prompt: string
  prompt_vars?: Record<string, string>
  schedule: string
  timeout?: string
  canary_group?: string
  /** Omit to keep the stored value on PATCH; false turns verification back on. */
  insecure_skip_verify?: boolean
  /** Omit to keep the stored value on PATCH; false disables thinking. */
  thinking_enabled?: boolean
  notify: {
    enabled: boolean
    channel?: string
    wecom_webhook?: string
  }
  openai?: { stream_path?: string; extra_headers?: string }
  anthropic?: { max_tokens?: number; extra_headers?: string }
  gemini?: { alt?: string; extra_headers?: string }
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