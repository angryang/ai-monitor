/**
 * Tiny global toast bus. Stays reactive so any component can call
 * `useToast().push(...)` and a single mounted Toast renderer picks them up.
 */

import { reactive } from 'vue'

export type ToastKind = 'info' | 'success' | 'warn' | 'error'

export interface Toast {
  id: number
  kind: ToastKind
  title: string
  detail?: string
  ttl: number
}

interface State {
  items: Toast[]
}

const state = reactive<State>({ items: [] })
let nextId = 1

function show(kind: ToastKind, title: string, detail?: string, ttl = 4200) {
  const t: Toast = { id: nextId++, kind, title, detail, ttl }
  state.items.push(t)
  setTimeout(() => dismiss(t.id), ttl)
}

function dismiss(id: number) {
  const idx = state.items.findIndex((x) => x.id === id)
  if (idx >= 0) state.items.splice(idx, 1)
}

export function useToast() {
  return {
    items: state.items,
    push(opts: { kind: ToastKind; title: string; detail?: string; ttl?: number }) {
      show(opts.kind, opts.title, opts.detail, opts.ttl)
    },
    success(title: string, detail?: string) { show('success', title, detail) },
    info(title: string, detail?: string) { show('info', title, detail) },
    warn(title: string, detail?: string) { show('warn', title, detail) },
    error(title: string, detail?: string) { show('error', title, detail) },
    dismiss,
  }
}