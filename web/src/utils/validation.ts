/**
 * Tiny form validators. Kept dependency-free so the bundle stays slim and
 * tests stay trivial.
 *
 * Validators read from the active i18n instance so error messages follow
 * the user's current language without callers having to translate.
 */

import { i18n } from '../i18n'

export interface ValidationResult {
  ok: boolean
  message?: string
}

const ok: ValidationResult = { ok: true }

/** Translation lookup that works whether or not i18n has been installed yet. */
function tt(key: string, params?: Record<string, unknown>): string {
  // `i18n.global.t` returns the key itself when missing, so callers still get
  // a usable string even before translations are wired in (e.g. tests).
  try {
    return String(i18n.global.t(key, params ?? {}))
  } catch {
    return key
  }
}

export function required(value: string | null | undefined, label = 'value'): ValidationResult {
  if (!value || !value.trim()) return { ok: false, message: tt('validation.requiredField', { label }) }
  return ok
}

export function minLength(value: string, n: number, label = 'value'): ValidationResult {
  if (value.length < n) return { ok: false, message: tt('validation.minLength', { n, label }) }
  return ok
}

export function url(value: string): ValidationResult {
  try {
    const u = new URL(value)
    if (!['http:', 'https:'].includes(u.protocol)) {
      return { ok: false, message: tt('validation.urlProtocol') }
    }
    return ok
  } catch {
    return { ok: false, message: tt('validation.url') }
  }
}

export function cron(value: string): ValidationResult {
  // 5-7 whitespace-separated fields. We accept the simple shape; deeper
  // validation lives server-side via robfig/cron.
  const parts = value.trim().split(/\s+/)
  if (parts.length < 5 || parts.length > 7) {
    return { ok: false, message: tt('validation.cron') }
  }
  return ok
}