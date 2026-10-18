/**
 * i18n bootstrap. Wires vue-i18n@9 in Composition mode and persists the
 * chosen language to localStorage so refreshes survive. Default is zh-CN,
 * matching the team's primary audience.
 *
 * Usage:
 *   import { useI18n } from 'vue-i18n'            // in components (Composition API)
 *   import { setLocale, type AppLocale } from '@/i18n'   // to switch language
 */

import { createI18n } from 'vue-i18n'
import zhCN from './locales/zh-CN.json'
import enUS from './locales/en-US.json'

export type AppLocale = 'zh-CN' | 'en-US'

export const STORAGE_KEY = 'ai-monitor.lang'

export const DEFAULT_LOCALE: AppLocale = 'zh-CN'

export const SUPPORTED_LOCALES: AppLocale[] = ['zh-CN', 'en-US']

export interface LocaleOption {
  code: AppLocale
  /** display name as rendered in the language switcher */
  label: string
  /** html lang attribute hint for document.documentElement.lang */
  htmlLang: string
}

export const LOCALE_OPTIONS: LocaleOption[] = [
  { code: 'zh-CN', label: '中文', htmlLang: 'zh-Hans' },
  { code: 'en-US', label: 'English', htmlLang: 'en' },
]

function isSupported(value: string | null): value is AppLocale {
  return !!value && (SUPPORTED_LOCALES as string[]).includes(value)
}

export function readStoredLocale(): AppLocale {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (isSupported(v)) return v
  } catch {
    /* localStorage may be unavailable (e.g. SSR / private mode) — ignore */
  }
  return DEFAULT_LOCALE
}

export function persistLocale(locale: AppLocale): void {
  try {
    localStorage.setItem(STORAGE_KEY, locale)
  } catch {
    /* best-effort */
  }
}

export function applyDocumentLang(locale: AppLocale): void {
  if (typeof document === 'undefined') return
  const opt = LOCALE_OPTIONS.find((o) => o.code === locale) ?? LOCALE_OPTIONS[0]
  document.documentElement.lang = opt.htmlLang
}

export const i18n = createI18n({
  legacy: false,
  globalInjection: true,
  locale: readStoredLocale(),
  fallbackLocale: DEFAULT_LOCALE,
  messages: {
    'zh-CN': zhCN,
    'en-US': enUS,
  },
})

/**
 * Switch the active locale at runtime. Also updates <html lang="…"> so screen
 * readers and CSS `:lang()` selectors react immediately.
 */
export function setLocale(next: AppLocale): void {
  i18n.global.locale.value = next
  persistLocale(next)
  applyDocumentLang(next)
}

// initial document.lang sync — done eagerly so first paint already has it right
applyDocumentLang(readStoredLocale())