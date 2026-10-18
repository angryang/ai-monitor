/**
 * Users API surface. Matches /admin/users endpoints.
 */

import { api } from './client'
import type { UserView } from './auth'

export interface CreateUserPayload {
  username: string
  display_name?: string
  password: string
  role: 'admin' | 'viewer'
  enabled?: boolean
}

export interface UpdateUserPayload {
  display_name?: string
  password?: string
  role?: 'admin' | 'viewer'
  enabled?: boolean
}

export const usersApi = {
  list() {
    return api.get<{ users: UserView[] }>('/admin/users')
  },
  get(id: string) {
    return api.get<UserView>(`/admin/users/${id}`)
  },
  create(payload: CreateUserPayload) {
    return api.post<UserView>('/admin/users', payload)
  },
  update(id: string, payload: UpdateUserPayload) {
    return api.patch<UserView>(`/admin/users/${id}`, payload)
  },
  remove(id: string) {
    return api.delete<{ ok: boolean }>(`/admin/users/${id}`)
  },
}