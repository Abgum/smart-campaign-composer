<script setup lang="ts">
import { t, tk } from '../i18n'
import { onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { api, ApiError } from '../api/http'
import type { Issue, ManualDraftInput, RuleCatalog } from '../api/types'
import CampaignEditor from '../components/campaign/CampaignEditor.vue'
import Icon from '../components/Icon.vue'

const router = useRouter()
const catalog = ref<RuleCatalog | null>(null)
const loadingError = ref('')
const saveError = ref('')
const issues = ref<Issue[]>([])
const saving = ref(false)
onMounted(async () => {
  try { catalog.value = await api.ruleCatalog() }
  catch { loadingError.value = t('Kitle kuralı seçenekleri yüklenemedi. Sayfayı yenileyin.') }
})
async function save(draft: ManualDraftInput) {
  saving.value = true
  saveError.value = ''
  issues.value = []
  try {
    const result = await api.createCampaign(draft)
    await router.push({ name: 'campaign', params: { id: result.campaign_id } })
  } catch (e) {
    saveError.value = e instanceof ApiError ? e.message : t('Kampanya kaydedilemedi; bağlantınızı kontrol edin.')
    issues.value = e instanceof ApiError ? e.body?.issues ?? [] : []
  } finally { saving.value = false }
}
</script>

<template>
  <section class="page manual-create-page">
    <RouterLink to="/campaigns" class="back-link"><Icon name="chevron" :size="16" />{{ t('Kampanyalar') }}</RouterLink>
    <header class="hero">
      <div class="hero-text"><h1 class="hero-title">{{ t('Yeni kampanya') }}</h1><p class="hero-sub">{{ t('Hedef kitleyi ve mesajları kendiniz hazırlayın. Kaydettiğiniz taslak onaylanana kadar gönderilmez.') }}</p></div>
    </header>
    <div class="manual-choice card">
      <div><strong>{{ t('Manuel oluşturma') }}</strong><p>{{ t('Asistan kullanılmaz. Hedef kitle kuralı ve mesajlarınız doğrulanarak taslak olarak kaydedilir.') }}</p></div>
      <RouterLink :to="{ name: 'workspace', query: { new: '1' } }" class="button">{{ t('Asistanla oluştur →') }}</RouterLink>
    </div>
    <p v-if="loadingError" class="error" role="alert">{{ loadingError }}</p>
    <div v-else-if="!catalog" class="card">{{ t('Kural seçenekleri yükleniyor…') }}</div>
    <div v-else class="card manual-editor-card">
      <CampaignEditor :initial="null" :catalog="catalog" :busy="saving" :error="saveError" :issues="issues" :save-label="tk('Taslağı oluştur')" @save="save" @cancel="router.push('/campaigns')" />
    </div>
  </section>
</template>
