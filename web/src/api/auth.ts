/**
 * Auth API surface. The token is owned by `stores/auth.ts`; this module
 * just shapes the request bodies and parses the responses.
 */

import { api } from './client'

export interface LoginResponse {
  token: string
  user: UserView
  expires_at: string
}

export interface UserView {
  id: string
  username: string
  display_name?: string
  role: 'admin' | 'viewer'
  enabled: boolean
  created_at: string
  updated_at: string
}

export const authApi = {
  login(username: string, password: string) {
    return api.post<LoginResponse>('/auth/login', { username, password })
  },
  logout() {
    return api.post<{ ok: boolean }>('/auth/logout')
  },
  me() {
    return api.get<UserView>('/auth/me')
  },
  changePassword(oldPassword: string, newPassword: string) {
    return api.post<{ ok: boolean }>('/auth/change-password', {
      old_password: oldPassword,
      new_password: newPassword,
    })
  },
}