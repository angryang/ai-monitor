/**
 * AI service provider API surface.
 *
 * A provider is the reusable description of one upstream AI endpoint:
 * protocol, base url, credentials, the set of models it offers and the
 * protocol-specific options. The server never returns the api key — only
 * `has_api_key` — so the form can be rendered without leaking the ciphertext.
 *
 * A provider exposes a list of candidate models (`models`). Requests and
 * monitoring tasks each pick exactly one of them; saving a provider no longer
 * runs a connectivity check, so there is no 422 / `force` save path anymore.
 * Connectivity checks are explicit, standalone calls instead.
 */

import { api } from './client'

export type Protocol = 'openai' | 'anthropic' | 'gemini'

/** How a connectivity check ended. */
export type CheckKind =
  | 'ok'
  | 'config_error'
  | 'unauthorized'
  | 'invalid_request'
  | 'upstream_unavailable'
  | 'timeout'
  | 'network_error'
  | 'unknown'

/** The verdict of one connectivity check. */
export interface ProviderCheck {
  ok: boolean
  kind: CheckKind
  protocol: string
  /** The single model that was actually tested by this check. */
  model: string
  url: string
  latency_ms: number
  status_code?: number
  message: string
  checked_at: string
}

/** One registered AI service provider. */
export interface Provider {
  id: string
  name: string
  description: string
  enabled: boolean
  protocol: Protocol
  base_url: string
  /** Candidate models offered by this provider (at least one, deduplicated). */
  models: string[]
  /** Human-readable duration, e.g. "30s". */
  timeout: string
  /** Per-provider switch: skip upstream TLS certificate verification. */
  insecure_skip_verify: boolean
  /** True when the instance-wide switch also skips verification. */
  insecure_skip_verify_global: boolean
  /** Per-provider default: ask the upstream model to emit its reasoning trace. */
  thinking_enabled: boolean
  has_api_key: boolean
  /**
   * Non-empty when the server could not decrypt this provider's stored key
   * with the current master key. The provider is listed but cannot be
   * used until the key is re-entered.
   */
  secret_error?: string
  openai?: { stream_path?: string; extra_headers?: string } | null
  anthropic?: { max_tokens?: number; extra_headers?: string } | null
  gemini?: { alt?: string; extra_headers?: string } | null
  /** When the provider was last verified against its upstream. */
  last_verified_at?: string | null
  /** Whether the last verification succeeded. */
  last_verified_ok: boolean
  /** Why the last verification failed (empty when it succeeded). */
  last_verified_message?: string
  /** The verdict of the check that ran with this request. */
  check?: ProviderCheck
  created_at: string
  updated_at: string
}

export interface ProviderWritePayload {
  name: string
  description?: string
  enabled?: boolean
  protocol: Protocol
  base_url: string
  /** Write-only. Omit or leave empty on PATCH to keep the stored key. */
  api_key?: string
  /**
   * The candidate models. Required on write: at least one, trimmed, non-empty
   * and deduplicated. The first entry is the primary / default model.
   */
  models: string[]
  /** e.g. "30s". Omit on PATCH to keep the stored timeout. */
  timeout?: string
  insecure_skip_verify?: boolean
  thinking_enabled?: boolean
  openai?: { stream_path?: string; extra_headers?: string }
  anthropic?: { max_tokens?: number; extra_headers?: string }
  gemini?: { alt?: string; extra_headers?: string }
}

export const providersApi = {
  list() {
    return api.get<{ providers: Provider[] }>('/admin/providers')
  },
  get(id: string) {
    return api.get<Provider>(`/admin/providers/${id}`)
  },
  create(payload: ProviderWritePayload) {
    return api.post<Provider>('/admin/providers', payload)
  },
  update(id: string, payload: Partial<ProviderWritePayload>) {
    return api.patch<Provider>(`/admin/providers/${id}`, payload)
  },
  remove(id: string) {
    return api.delete<{ ok: boolean }>(`/admin/providers/${id}`)
  },
  /**
   * Verify a provider described by the payload — nothing is saved. The body
   * is the full write payload; `model` optionally names which candidate model
   * to test (the server defaults to the first one).
   */
  check(payload: ProviderWritePayload, model?: string) {
    return api.post<ProviderCheck>(
      '/admin/providers/check',
      model ? { ...payload, model } : payload,
    )
  },
  /**
   * Verify a saved provider with its stored credentials. `model` optionally
   * names which candidate model to test; when omitted the POST has no body and
   * the server tests the first model.
   */
  checkStored(id: string, model?: string) {
    return api.post<ProviderCheck>(
      `/admin/providers/${id}/check`,
      model ? { model } : undefined,
    )
  },
}
