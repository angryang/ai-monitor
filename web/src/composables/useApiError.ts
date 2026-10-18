/**
 * useApiError — translates ApiError into a human-readable message so
 * views can render form-level errors without re-typing the map.
 */

import { computed, ref, type Ref } from 'vue'
import { ApiError } from '../api/client'
import { i18n } from '../i18n'

function tt(key: string, params?: Record<string, unknown>): string {
  try {
    return String(i18n.global.t(key, params ?? {}))
  } catch {
    return key
  }
}

export function useApiError() {
  const message: Ref<string | null> = ref(null)
  const code: Ref<string | null> = ref(null)
  const status: Ref<number | null> = ref(null)

  function capture(e: unknown) {
    if (e instanceof ApiError) {
      status.value = e.status
      code.value = e.code
      message.value = e.message
    } else if (e instanceof Error) {
      message.value = e.message
      code.value = 'unknown'
      status.value = null
    } else {
      message.value = tt('common.unknownError')
      code.value = 'unknown'
      status.value = null
    }
  }

  function reset() {
    message.value = null
    code.value = null
    status.value = null
  }

  const hasError = computed(() => !!message.value)

  return { message, code, status, hasError, capture, reset }
}