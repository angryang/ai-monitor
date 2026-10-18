/**
 * Pinia auth store. Persists the JWT + user in localStorage so refreshes
 * survive without bouncing the user back to /login every time.
 */

import { defineStore } from 'pinia'
import { authApi, type UserView } from '../api/auth'

interface State {
  token: string | null
  user: UserView | null
  expiresAt: number | null
}

const STORAGE_KEY = 'ai-monitor.auth.v1'

interface Persisted {
  token: string
  user: UserView
  expiresAt: number
}

function loadPersisted(): Persisted | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return null
    const parsed = JSON.parse(raw) as Persisted
    if (!parsed.token || !parsed.user || parsed.expiresAt < Date.now()) {
      return null
    }
    return parsed
  } catch {
    return null
  }
}

function savePersisted(state: State) {
  if (!state.token || !state.user || !state.expiresAt) {
    localStorage.removeItem(STORAGE_KEY)
    return
  }
  const data: Persisted = {
    token: state.token,
    user: state.user,
    expiresAt: state.expiresAt,
  }
  localStorage.setItem(STORAGE_KEY, JSON.stringify(data))
}

export const useAuthStore = defineStore('auth', {
  state: (): State => {
    const persisted = loadPersisted()
    if (persisted) {
      return {
        token: persisted.token,
        user: persisted.user,
        expiresAt: persisted.expiresAt,
      }
    }
    return { token: null, user: null, expiresAt: null }
  },
  getters: {
    isAuthenticated: (s): boolean => !!s.token && !!s.user && (s.expiresAt ?? 0) > Date.now(),
    isAdmin: (s): boolean => s.user?.role === 'admin',
    isViewer: (s): boolean => s.user?.role === 'viewer',
  },
  actions: {
    async login(username: string, password: string) {
      const resp = await authApi.login(username, password)
      this.token = resp.token
      this.user = resp.user
      this.expiresAt = new Date(resp.expires_at).getTime()
      savePersisted(this.$state)
    },
    async refresh() {
      try {
        const u = await authApi.me()
        this.user = u
        savePersisted(this.$state)
      } catch (e) {
        this.clear()
        throw e
      }
    },
    async logout() {
      try {
        await authApi.logout()
      } catch {
        // even if the server 500s, we still want to clear the token client-side
      }
      this.clear()
    },
    clear() {
      this.token = null
      this.user = null
      this.expiresAt = null
      localStorage.removeItem(STORAGE_KEY)
    },
  },
})