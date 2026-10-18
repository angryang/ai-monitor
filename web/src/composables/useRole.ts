/**
 * useRole — composable bridge so views can check the current user's role
 * without importing the Pinia store directly.
 */

import { computed } from 'vue'
import { useAuthStore } from '../stores/auth'

export function useRole() {
  const auth = useAuthStore()
  const isAdmin = computed(() => auth.isAdmin)
  const isViewer = computed(() => auth.isViewer)
  const isAuthenticated = computed(() => auth.isAuthenticated)
  const user = computed(() => auth.user)
  return { isAdmin, isViewer, isAuthenticated, user }
}