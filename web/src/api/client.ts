/**
 * Centralised fetch wrapper. Adds bearer token, JSON encodes the body, and
 * surfaces the canonical APIError envelope (`{error: {code, message}}`) as
 * a typed rejection so callers can branch on `code` without parsing
 * strings.
 *
 * Two interceptors matter:
 *   - 401 → clear the auth store + redirect to /login?next=<currentPath>.
 *   - 403 → keep the user where they are; emit a toast so they know.
 */

import { useAuthStore } from '../stores/auth'
import { useToast } from '../utils/toast'
import { i18n } from '../i18n'

function tt(key: string, params?: Record<string, unknown>): string {
  try {
    return String(i18n.global.t(key, params ?? {}))
  } catch {
    return key
  }
}

export interface ApiErrorPayload {
  code: string
  message: string
}

export class ApiError extends Error {
  status: number
  code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.status = status
    this.code = code
    this.name = 'ApiError'
  }
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'
  body?: unknown
  query?: Record<string, string | number | undefined>
  headers?: Record<string, string>
  auth?: boolean
}

function buildURL(path: string, query?: RequestOptions['query']): string {
  const url = new URL(path, window.location.origin)
  if (query) {
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined) continue
      url.searchParams.set(k, String(v))
    }
  }
  return url.toString()
}

export async function apiFetch<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const auth = useAuthStore()
  const toast = useToast()
  const headers: Record<string, string> = {
    Accept: 'application/json',
    ...(opts.headers ?? {}),
  }
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }
  const needsAuth = opts.auth !== false
  if (needsAuth && auth.token) {
    headers['Authorization'] = `Bearer ${auth.token}`
  }

  const url = buildURL(path, opts.query)
  const init: RequestInit = {
    method: opts.method ?? (opts.body !== undefined ? 'POST' : 'GET'),
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : null,
  }
  const res = await fetch(url, init)

  if (res.status === 401) {
    auth.clear()
    const next = encodeURIComponent(window.location.pathname + window.location.search)
    if (window.location.pathname !== '/login') {
      window.history.replaceState({}, '', `/login?next=${next}`)
      window.location.assign(`/login?next=${next}`)
    }
    throw new ApiError(401, 'unauthorized', 'authentication required')
  }
  if (res.status === 403) {
    const detail = tt('forbidden.desc', { path: tt('forbidden.fallbackPath') })
    toast.push({ kind: 'warn', title: tt('forbidden.title'), detail })
    throw new ApiError(403, 'forbidden', detail)
  }

  const text = await res.text()
  let payload: unknown = null
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch (e) {
      payload = text
    }
  }
  if (!res.ok) {
    const err = (payload as { error?: ApiErrorPayload })?.error
    throw new ApiError(
      res.status,
      err?.code ?? `http_${res.status}`,
      err?.message ?? text ?? res.statusText,
    )
  }
  return payload as T
}

export const api = {
  get: <T>(p: string, q?: RequestOptions['query']) => apiFetch<T>(p, { method: 'GET', query: q }),
  post: <T>(p: string, body?: unknown) => apiFetch<T>(p, { method: 'POST', body }),
  patch: <T>(p: string, body?: unknown) => apiFetch<T>(p, { method: 'PATCH', body }),
  delete: <T>(p: string) => apiFetch<T>(p, { method: 'DELETE' }),
}