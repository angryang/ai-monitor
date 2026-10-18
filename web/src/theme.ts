/**
 * Theme bootstrap. Mirrors the i18n module shape on purpose: a single
 * `AppTheme` type, a `STORAGE_KEY` constant, a `readStoredTheme()` / `setTheme()`
 * pair, and a one-shot `applyDocumentTheme()` that runs at module load so the
 * very first paint already has the right palette.
 *
 * The CSS side lives in `src/styles/tokens.css`:
 *   - `:root { … }`               is the default (dark) palette
 *   - `[data-theme="light"] { … }` overrides the same tokens
 *
 * We toggle the theme by setting `document.documentElement.dataset.theme`
 * rather than swapping a stylesheet — that way every `var(--…)` in the app
 * reactively reflows without a flash of unstyled content.
 *
 * `themeRef` is the single source of truth for the *current* theme. The DOM
 * `data-theme` attribute is a pure reflection of it; reading `dataset.theme`
 * from a Vue computed would never trigger updates because Vue's reactivity
 * does not track DOMStringMap mutations. Keeping a `Ref<AppTheme>` here makes
 * `setTheme()` and any UI that consumes the current theme both reactive.
 *
 * Usage:
 *   import { themeRef, setTheme, type AppTheme } from '@/theme'
 *   // `themeRef.value` is the current theme; setTheme(next) updates ref + DOM + storage.
 */

import { ref, type Ref } from 'vue'

export type AppTheme = 'dark' | 'light'

export const STORAGE_KEY = 'ai-monitor.theme'

export const DEFAULT_THEME: AppTheme = 'dark'

export const SUPPORTED_THEMES: AppTheme[] = ['dark', 'light']

function isSupported(value: string | null): value is AppTheme {
  return !!value && (SUPPORTED_THEMES as string[]).includes(value)
}

export function readStoredTheme(): AppTheme {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (isSupported(v)) return v
  } catch {
    /* localStorage may be unavailable (SSR / private mode) — fall through */
  }
  return DEFAULT_THEME
}

export function persistTheme(theme: AppTheme): void {
  try {
    localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    /* best-effort */
  }
}

export function applyDocumentTheme(theme: AppTheme): void {
  if (typeof document === 'undefined') return
  // The dark palette lives under :root, so we explicitly tag the element to
  // keep the attribute round-trippable. `data-theme` is the single source of
  // truth — components should never branch on prefers-color-scheme.
  if (theme === 'light') {
    document.documentElement.dataset.theme = 'light'
  } else {
    delete document.documentElement.dataset.theme
  }
}

// Module-level reactive source of truth. Initialised from storage so the first
// paint already matches user intent. All UI reads `themeRef.value`; all writes
// go through `setTheme()` so ref + DOM + storage stay in lock-step.
export const themeRef: Ref<AppTheme> = ref<AppTheme>(readStoredTheme())

export function setTheme(next: AppTheme): void {
  themeRef.value = next
  applyDocumentTheme(next)
  persistTheme(next)
}

// Cross-tab sync: when another tab toggles the theme, localStorage fires a
// `storage` event (the tab that wrote does *not* receive it). Keep all three
// representations in sync without forcing the user to refresh.
if (typeof window !== 'undefined') {
  window.addEventListener('storage', (e) => {
    if (e.key !== STORAGE_KEY) return
    if (!isSupported(e.newValue)) return
    themeRef.value = e.newValue
    applyDocumentTheme(e.newValue)
  })
}

// Initial sync — runs once on import so the first paint already matches the
// stored choice. Subsequent changes go through setTheme() / the switcher.
applyDocumentTheme(themeRef.value)