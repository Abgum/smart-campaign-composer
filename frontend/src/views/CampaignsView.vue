<script setup lang="ts">
import { t, tk } from '../i18n'
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { useCampaigns } from '../lib/data'
import { useLiveStore } from '../stores/live'
import Icon from '../components/Icon.vue'
import CampaignTable from '../components/overview/CampaignTable.vue'
import EmptyState from '../components/overview/EmptyState.vue'

// Every campaign of every chat. The status and the search live in the URL
// with the API's names (?status=draft|active|completed&q=…).
const route = useRoute()
const router = useRouter()
const live = useLiveStore()

const statuses = [
  { value: '', label: tk('Tümü') },
  { value: 'draft', label: tk('Taslak') },
  { value: 'active', label: tk('Gönderimde') },
  { value: 'completed', label: tk('Tamamlandı') },
]
const status = computed(() => statuses.find((s) => s.value === route.query.status) ?? statuses[0])
const search = ref(String(route.query.q ?? ''))
let timer: ReturnType<typeof setTimeout> | undefined
watch(search, (q) => {
  clearTimeout(timer)
  timer = setTimeout(() => void router.replace({ query: { ...route.query, q: q.trim() || undefined } }), 300)
})

const query = () => {
  const q = new URLSearchParams({ limit: '200' })
  if (status.value.value) q.set('status', status.value.value)
  const term = String(route.query.q ?? '').trim()
  if (term) q.set('q', term)
  return q.toString()
}
const campaigns = useCampaigns(query, () => live.version)
const filtered = computed(() => !!status.value.value || !!route.query.q)
</script>

<template>
  <section class="page campaigns-page">
    <header class="hero">
      <div class="hero-text">
        <h1 class="hero-title">{{ t('Kampanyalar') }}</h1>
        <p class="hero-sub">{{ t('Tüm sohbetlerdeki kampanyalar: durumu, gönderimin ne kadar ilerlediği ve onayda kimlerin elendiği.') }}</p>
      </div>
      <div class="hero-actions">
        <RouterLink :to="{ name: 'campaign-new' }" class="button primary hero-new">
          <Icon name="plus" :size="16" />{{ t('Yeni kampanya') }}
        </RouterLink>
      </div>
    </header>

    <div class="card filter-card">
      <div class="filter-group">
        <span id="filter-status" class="filter-label">{{ t('Durum') }}</span>
        <div class="segmented" role="group" aria-labelledby="filter-status">
          <button
            v-for="s in statuses"
            :key="s.value"
            type="button"
            :data-status="s.value || 'all'"
            :aria-pressed="status.value === s.value"
            @click="router.replace({ query: { ...route.query, status: s.value || undefined } })"
          >
            {{ t(s.label) }}
          </button>
        </div>
      </div>
      <div class="filter-group grow">
        <label class="filter-label" for="campaign-search">{{ t('Ara') }}</label>
        <input id="campaign-search" v-model="search" type="search" :placeholder="t('Kampanya adı…')" />
      </div>
    </div>

    <EmptyState
      v-if="campaigns.data.value && !campaigns.list().length && !filtered"
      icon="campaigns"
      :title="t('Henüz kampanya yok')"
      :text="t('Asistana kampanya isteğinizi yazın. Taslak burada listelenir; onaydan sonra gönderimi izleyebilirsiniz.')"
      :action="{ label: t('Yeni kampanya'), to: { name: 'campaign-new' } }"
    />
    <div v-else class="card table-card" :class="{ refreshing: campaigns.loading.value }">
      <CampaignTable :campaigns="campaigns.list()" />
    </div>
  </section>
</template>
