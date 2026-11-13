/**
 * AI service provider store. Holds the registry, the verdict of the last
 * connectivity check, and the busy flags the list view needs.
 */

import { defineStore } from 'pinia'
import {
  providersApi,
  type Provider,
  type ProviderCheck,
  type ProviderWritePayload,
} from '../api/providers'

interface State {
  providers: Provider[]
  loading: boolean
  lastError: string | null
  /** True while a provider is being saved. */
  saving: boolean
  /** True while a connectivity check is running. */
  checking: boolean
  /** The verdict of the most recent check, success or failure. */
  lastCheck: ProviderCheck | null
}

export const useProvidersStore = defineStore('providers', {
  state: (): State => ({
    providers: [],
    loading: false,
    lastError: null,
    saving: false,
    checking: false,
    lastCheck: null,
  }),
  getters: {
    /** Providers whose stored key cannot be decrypted with the current key. */
    brokenProviders(state): Provider[] {
      return state.providers.filter((p) => !!p.secret_error)
    },
    /** Providers that were verified against their upstream at least once. */
    verifiedProviders(state): Provider[] {
      return state.providers.filter((p) => p.last_verified_ok)
    },
  },
  actions: {
    async fetchAll() {
      this.loading = true
      this.lastError = null
      try {
        const { providers } = await providersApi.list()
        this.providers = providers ?? []
      } catch (e) {
        this.lastError = (e as Error).message
      } finally {
        this.loading = false
      }
    },

    async create(payload: ProviderWritePayload): Promise<Provider> {
      this.saving = true
      try {
        const p = await providersApi.create(payload)
        this.providers = [...this.providers, p]
        if (p.check) this.lastCheck = p.check
        return p
      } finally {
        this.saving = false
      }
    },

    async update(id: string, payload: Partial<ProviderWritePayload>): Promise<Provider> {
      this.saving = true
      try {
        const p = await providersApi.update(id, payload)
        this.providers = this.providers.map((x) => (x.id === id ? p : x))
        if (p.check) this.lastCheck = p.check
        return p
      } finally {
        this.saving = false
      }
    },

    async remove(id: string) {
      await providersApi.remove(id)
      this.providers = this.providers.filter((x) => x.id !== id)
    },

    /** Verify a provider described by the form — nothing is saved. */
    async check(payload: ProviderWritePayload, model?: string): Promise<ProviderCheck> {
      this.checking = true
      try {
        const res = await providersApi.check(payload, model)
        this.lastCheck = res
        return res
      } finally {
        this.checking = false
      }
    },

    /** Verify a saved provider with its stored credentials. */
    async checkStored(id: string, model?: string): Promise<ProviderCheck> {
      this.checking = true
      try {
        const res = await providersApi.checkStored(id, model)
        this.lastCheck = res
        // Reflect the fresh verdict in the list so the badge stays honest.
        this.providers = this.providers.map((p) =>
          p.id === id
            ? {
                ...p,
                last_verified_ok: res.ok,
                last_verified_at: res.checked_at,
                last_verified_message: res.ok ? '' : res.message,
              }
            : p,
        )
        return res
      } finally {
        this.checking = false
      }
    },
  },
})
