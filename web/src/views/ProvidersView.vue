<script setup lang="ts">
/**
 * AI service provider registry.
 *
 * A provider describes one upstream AI endpoint and the set of models it
 * offers. The list shows what is registered and whether each entry was
 * verified against its upstream. Connectivity tests are explicit actions
 * (per model) — saving a provider never runs one.
 */

import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useProvidersStore } from '../stores/providers'
import { useRole } from '../composables/useRole'
import { useToast } from '../utils/toast'
import Badge from '../components/ui/Badge.vue'
import Button from '../components/ui/Button.vue'
import EmptyState from '../components/ui/EmptyState.vue'
import ProviderFormModal from '../components/providers/ProviderFormModal.vue'
import { ApiError } from '../api/client'
import type { Provider, ProviderCheck, ProviderWritePayload } from '../api/providers'

const { t } = useI18n()
const store = useProvidersStore()
const toast = useToast()
const { isAdmin } = useRole()

onMounted(() => store.fetchAll())

// ---- form ----
const modalOpen = ref(false)
const beingEdited = ref<Provider | null>(null)
/** The verdict of the last check, shown inside the form. */
const checkResult = ref<ProviderCheck | null>(null)

function openNew() {
  beingEdited.value = null
  checkResult.value = null
  modalOpen.value = true
}

function openEdit(p: Provider) {
  beingEdited.value = p
  checkResult.value = null
  modalOpen.value = true
}

async function submit(payload: ProviderWritePayload) {
  try {
    if (beingEdited.value) {
      await store.update(beingEdited.value.id, payload)
      toast.success(t('toast.providerUpdated'))
    } else {
      await store.create(payload)
      toast.success(t('toast.providerCreated'))
    }
    checkResult.value = null
    modalOpen.value = false
  } catch (e) {
    const err = e as ApiError
    if (err.code === 'model_in_use') {
      // A removed model is still referenced by requests / tasks — surface
      // the affected names from the error details.
      const d = (err.details ?? {}) as { requests?: string[]; tasks?: string[] }
      const reqs = d.requests ?? []
      const tsks = d.tasks ?? []
      const detail = t('toast.modelInUseDetail', {
        requests: reqs.length ? reqs.join('、') : '—',
        tasks: tsks.length ? tsks.join('、') : '—',
      })
      toast.error(t('toast.modelInUse'), detail)
      return
    }
    toast.error(t('toast.errSave'), err.message)
  }
}

async function runCheck(payload: ProviderWritePayload, model?: string) {
  try {
    checkResult.value = await store.check(payload, model)
  } catch (e) {
    toast.error(t('toast.errCheck'), (e as Error).message)
  }
}

async function remove(p: Provider) {
  if (!window.confirm(t('provider.deleteConfirm', { name: p.name }))) return
  try {
    await store.remove(p.id)
    toast.success(t('toast.providerDeleted'))
  } catch (e) {
    toast.error(t('toast.errDelete'), (e as Error).message)
  }
}

/** Re-verify a saved provider's primary model with its stored credentials. */
async function verify(p: Provider) {
  const model = p.models?.[0] ?? ''
  try {
    const res = await store.checkStored(p.id, model)
    if (res.ok) {
      toast.success(t('provider.verifyOk', { name: p.name, model: res.model, ms: res.latency_ms }))
    } else {
      toast.error(t('provider.verifyFailed', { name: p.name, model: res.model }), res.message)
    }
  } catch (e) {
    toast.error(t('toast.errCheck'), (e as Error).message)
  }
}

/** The badge tone of a provider's last verification. */
function verifyTone(p: Provider): 'ok' | 'warn' | 'danger' | 'muted' {
  if (p.secret_error) return 'danger'
  if (!p.last_verified_at) return 'muted'
  return p.last_verified_ok ? 'ok' : 'danger'
}

function verifyLabel(p: Provider): string {
  if (p.secret_error) return t('provider.status.broken')
  if (!p.last_verified_at) return t('provider.status.unverified')
  return p.last_verified_ok ? t('provider.status.verified') : t('provider.status.failed')
}

const verifiedCount = computed(() => store.verifiedProviders.length)
</script>

<template>
  <section class="provs">
    <header class="provs__head">
      <div>
        <h1>{{ t('provider.list.title') }}</h1>
        <p class="text-muted">{{ t('provider.list.subtitle') }}</p>
      </div>
      <div class="provs__actions">
        <Button v-if="isAdmin" @click="openNew">{{ t('provider.list.new') }}</Button>
      </div>
    </header>

    <div v-if="store.loading" class="text-muted">{{ t('common.loading') }}</div>

    <template v-else>
      <div v-if="store.brokenProviders.length > 0" class="provs__alert" role="alert">
        <strong>{{ t('provider.list.secretErrorTitle') }}</strong>
        <span>{{
          t('provider.list.secretErrorDesc', {
            names: store.brokenProviders.map((p) => p.name).join('、'),
          })
        }}</span>
      </div>

      <EmptyState
        v-if="store.providers.length === 0"
        :title="t('provider.list.emptyTitle')"
        :description="t('provider.list.emptyDesc')"
        glyph="◈"
      >
        <template #action>
          <Button v-if="isAdmin" @click="openNew">{{ t('provider.list.new') }}</Button>
        </template>
      </EmptyState>

      <article v-for="p in store.providers" :key="p.id" class="prov">
        <header class="prov__head">
          <div class="prov__title">
            <h2 class="prov__name">{{ p.name }}</h2>
            <Badge tone="accent">{{ p.protocol }}</Badge>
            <Badge :tone="verifyTone(p)">{{ verifyLabel(p) }}</Badge>
            <span class="prov__models">
              <Badge v-for="(m, i) in p.models" :key="i" :tone="i === 0 ? 'accent' : 'muted'">
                {{ m }}
              </Badge>
            </span>
          </div>
          <div class="prov__actions">
            <Button v-if="isAdmin" size="sm" variant="ghost" :loading="store.checking" @click="verify(p)">
              {{ t('provider.list.verify') }}
            </Button>
            <Button v-if="isAdmin" size="sm" variant="ghost" @click="openEdit(p)">
              {{ t('common.edit') }}
            </Button>
            <Button v-if="isAdmin" size="sm" variant="danger" @click="remove(p)">
              {{ t('common.delete') }}
            </Button>
          </div>
        </header>

        <p v-if="p.description" class="prov__desc">{{ p.description }}</p>

        <dl class="prov__meta">
          <dt>{{ t('provider.form.baseUrl') }}</dt>
          <dd class="text-mono">{{ p.base_url }}</dd>
          <dt>{{ t('provider.form.timeout') }}</dt>
          <dd class="text-mono">{{ p.timeout }}</dd>
          <dt>{{ t('provider.form.apiKey') }}</dt>
          <dd>
            <span v-if="p.secret_error" class="prov__secret-error">{{ t('provider.status.broken') }}</span>
            <span v-else>{{ p.has_api_key ? t('provider.form.configured') : t('provider.form.notSet') }}</span>
          </dd>
          <dt>{{ t('provider.list.state') }}</dt>
          <dd>{{ p.enabled ? t('common.enabled') : t('common.disabled') }}</dd>
          <dt>{{ t('provider.list.verifiedAt') }}</dt>
          <dd class="text-mono">
            {{ p.last_verified_at ? new Date(p.last_verified_at).toLocaleString() : '—' }}
          </dd>
        </dl>

        <p v-if="p.last_verified_message && !p.last_verified_ok" class="prov__last-error">
          {{ p.last_verified_message }}
        </p>
      </article>

      <footer v-if="store.providers.length > 0" class="provs__foot text-muted text-mono">
        {{ t('provider.list.summary', { total: store.providers.length, verified: verifiedCount }) }}
      </footer>
    </template>

    <ProviderFormModal
      :open="modalOpen"
      :provider="beingEdited"
      :busy="store.saving"
      :checking="store.checking"
      :check-result="checkResult"
      @close="modalOpen = false"
      @submit="submit"
      @check="runCheck"
    />
  </section>
</template>

<style scoped>
.provs { display: flex; flex-direction: column; gap: var(--space-5); }
.provs__head {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  gap: var(--space-4);
}
.provs__actions { display: flex; gap: var(--space-3); }
.provs__alert {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-4);
  border: 1px solid var(--danger);
  border-radius: var(--radius-lg);
  background: rgba(216, 68, 63, 0.12);
  font-size: var(--fs-13);
  line-height: var(--lh-base);
}
.provs__alert strong { color: var(--danger); font-weight: var(--fw-semibold); }
.provs__foot { font-size: var(--fs-11); }

.prov {
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  background: var(--surface);
  padding: var(--space-4) var(--space-5) var(--space-5);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.prov__head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: var(--space-4);
  flex-wrap: wrap;
}
.prov__title { display: flex; align-items: center; gap: var(--space-3); flex-wrap: wrap; }
.prov__name {
  margin: 0;
  font-size: var(--fs-16);
  font-family: var(--font-display);
  font-weight: var(--fw-semibold);
}
.prov__models { display: inline-flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
.prov__actions { display: flex; gap: var(--space-2); }
.prov__desc { margin: 0; font-size: var(--fs-13); color: var(--muted); }
.prov__meta {
  display: grid;
  grid-template-columns: 120px 1fr;
  gap: var(--space-2) var(--space-4);
  margin: 0;
}
.prov__meta dt {
  color: var(--muted);
  font-size: var(--fs-11);
  text-transform: uppercase;
  letter-spacing: 0.06em;
  font-family: var(--font-mono);
}
.prov__meta dd { margin: 0; font-size: var(--fs-13); }
.prov__secret-error { color: var(--danger); font-weight: var(--fw-semibold); }
.prov__last-error {
  margin: 0;
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--danger);
  border-radius: var(--radius-md);
  background: rgba(216, 68, 63, 0.08);
  font-family: var(--font-mono);
  font-size: var(--fs-11);
  color: var(--muted);
  word-break: break-all;
}
</style>
