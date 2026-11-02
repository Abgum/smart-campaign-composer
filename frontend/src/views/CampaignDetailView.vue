<script setup lang="ts">
import { locale, t } from '../i18n'
import { computed, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { api, ApiError } from '../api/http'
import type { ApproveOptions, CampaignDetail, Issue, Localization, ManualDraftInput, RuleCatalog, TrackedCampaign } from '../api/types'
import { dateTimeText, scheduledLater, statusLabel } from '../lib/schedule'
import { analyticsQuery } from '../lib/filters'
import { campaignTotals, foldCities } from '../lib/campaigns'
import { useAnalytics, useCampaigns, useMedia } from '../lib/data'
import { channelName, conditionText, formatCount, orderedChannels, relativeTime } from '../lib/format'
import { useLiveStore } from '../stores/live'
import Icon from '../components/Icon.vue'
import StatTile from '../components/charts/StatTile.vue'
import StackedColumns from '../components/charts/StackedColumns.vue'
import CityBars from '../components/charts/CityBars.vue'
import AbResults from '../components/campaign/AbResults.vue'
import LanguagePreview from '../components/assistant/LanguagePreview.vue'
import CampaignEditor from '../components/campaign/CampaignEditor.vue'
import ApproveModal from '../components/ApproveModal.vue'
import ConfirmDialog from '../components/ConfirmDialog.vue'

// One campaign on its own page: how far sending got, per outcome and
// channel, who was left out at approval, sends by day and by city since it
// started, and the content as approved. Every number comes from stored data
// (the campaign list, its record and the analytics for this campaign), and
// follows the live stream like the overview.
const route = useRoute()
const live = useLiveStore()
const phone = useMedia('(max-width: 767px)')
const version = () => live.version
const id = computed(() => String(route.params.id ?? ''))

const detail = ref<CampaignDetail | null>(null)
const missing = ref(false)
const editing = ref(false)
const catalog = ref<RuleCatalog | null>(null)
const editError = ref('')
const editIssues = ref<Issue[]>([])
const saving = ref(false)
const dirty = ref(false)
const showApprove = ref(false)
const staleConflict = ref(false)
const refreshPending = ref(false)
const saveNotice = ref('')
const askCancel = ref(false)
const cancelError = ref('')
const cancelling = ref(false)
async function load() {
  try {
    detail.value = await api.campaign(id.value)
    missing.value = false
  } catch (e) {
    detail.value = null
    missing.value = e instanceof ApiError && e.status === 404
  }
}
watch(id, load, { immediate: true })
watch(id, () => { editing.value = false; showApprove.value = false; askCancel.value = false; refreshPending.value = false; saveNotice.value = '' })
async function startEdit() {
  if (detail.value?.status !== 'draft' || refreshPending.value) return
  editError.value = ''
  staleConflict.value = false
  try { catalog.value ??= await api.ruleCatalog(); editing.value = true }
  catch { editError.value = t('Kitle kuralı seçenekleri yüklenemedi.') }
}
async function saveEdit(draft: ManualDraftInput) {
  if (!detail.value?.current_version || detail.value.status !== 'draft') return
  saving.value = true
  editError.value = ''
  editIssues.value = []
  staleConflict.value = false
  try {
    await api.editCampaign(detail.value.id, { base_version_id: detail.value.current_version.id, ...draft })
    editing.value = false
    dirty.value = false
    await refreshSaved()
  } catch (e) {
    editError.value = e instanceof ApiError ? e.message : t('Taslak kaydedilemedi.')
    editIssues.value = e instanceof ApiError ? e.body?.issues ?? [] : []
    if (e instanceof ApiError && e.status === 409) staleConflict.value = true
  } finally { saving.value = false }
}
// Cancelling asks first in a dialog, which stays open until the request ends.
async function cancelScheduled() {
  if (!detail.value || !waitsForTime.value || cancelling.value) return
  cancelling.value = true
  cancelError.value = ''
  try {
    await api.cancelScheduled(detail.value.id)
    await load()
  } catch (e) {
    cancelError.value = e instanceof ApiError ? e.message : t('Kampanya iptal edilemedi.')
    await load()
  } finally {
    cancelling.value = false
    askCancel.value = false
  }
}
// A language decision made in the approval dialog is a new version; the
// dialog then shows the preview of that version.
async function saveDecisions(versionId: number, localization: Localization) {
  const v = detail.value?.current_version
  if (!detail.value || !v) return
  await api.editCampaign(detail.value.id, { base_version_id: versionId, name: v.name, segment_rule: v.segment_rule, sms: null, email: null, localization })
  await load()
}

async function refreshSaved() {
  try {
    detail.value = await api.campaign(id.value)
    refreshPending.value = false
    saveNotice.value = ''
  } catch {
    // The POST committed; preserve the last visible detail until a GET works.
    refreshPending.value = true
    saveNotice.value = t('Taslak kaydedildi, ancak görünüm yenilenemedi. Yeniden deneyin.')
  }
}
async function reloadConflict() {
  try {
    // Only an explicit reload replaces the editor's base version and fields.
    detail.value = await api.campaign(id.value)
    staleConflict.value = false
    editError.value = ''
    editIssues.value = []
    dirty.value = false
  } catch { editError.value = t('Güncel sürüm yüklenemedi; değişiklikleriniz korunuyor.') }
}
// The tab names the campaign, not just "Kampanya".
watch([detail, locale], ([d]) => d && (document.title = `${d.name} · ${t('Kampanyalar')} · Smart Campaign Composer`))

const list = useCampaigns(() => `id=${id.value}`, version)
const tracked = computed<TrackedCampaign | null>(() => list.list()[0] ?? null)
// Approval changes what the page can show: read the record again then.
watch(
  () => tracked.value?.status,
  (now, before) => before !== undefined && now !== before && void load(),
)

const sent = computed(() => !!detail.value && detail.value.status !== 'draft')
// A scheduled campaign is approved but sends nothing before its time; its
// messages wait apart from the queue until then.
const scheduledAt = computed(() => detail.value?.scheduled_at ?? tracked.value?.scheduled_at ?? null)
const waitsForTime = computed(() => detail.value?.status === 'approved' && scheduledLater(scheduledAt.value))
const analytics = useAnalytics(() => (sent.value ? analyticsQuery({ range: 'all', channel: '', campaign: id.value }, 10) : ''), version)
const data = computed(() => analytics.data.value)
const cities = computed(() => (data.value ? (phone.value ? foldCities(data.value.cities, 5) : data.value.cities) : null))

const totals = computed(() => (tracked.value ? campaignTotals(tracked.value) : null))
function split(key: 'sent' | 'queued' | 'failed' | 'unknown') {
  const c = tracked.value
  if (!c) return null
  const one = (ch: string) => (c.channels[ch] ? campaignTotals({ ...c, channels: { [ch]: c.channels[ch] } })[key] : 0)
  return { sms: one('sms'), email: one('email') }
}

const content = computed(() => detail.value?.current_version ?? null)
const approval = computed(() =>
  orderedChannels(Object.keys(detail.value?.approval ?? {})).map((ch) => {
    const a = detail.value!.approval![ch]
    const parts = [t('{n} kişi seçildi', { n: formatCount(a.selected) })]
    if (a.excluded_consent) parts.push(t('{n} kişi izin yok', { n: formatCount(a.excluded_consent) }))
    if (a.excluded_duplicate) parts.push(t('{n} kişi içerik tekrarı', { n: formatCount(a.excluded_duplicate) }))
    return { ch, text: parts.join(' · ') }
  }),
)
const source = computed(() => {
  const v = content.value
  if (!v) return ''
  return v.source === 'llm' ? (v.llm_provider === 'mock' ? 'MOCK LLM' : 'Claude') : v.version === 1 ? t('Manuel olarak oluşturuldu') : t('Manuel olarak düzenlendi')
})
</script>

<template>
  <section class="page campaign-detail">
    <RouterLink to="/campaigns" class="back-link"><Icon name="chevron" :size="16" />{{ t('Kampanyalar') }}</RouterLink>

    <div v-if="missing" class="card empty-state">
      <span class="empty-icon" aria-hidden="true"><Icon name="campaigns" :size="24" /></span>
      <h2>{{ t('Kampanya bulunamadı') }}</h2>
      <p>{{ t('Bu adreste bir kampanya yok; silinmiş ya da adres yanlış yazılmış olabilir.') }}</p>
    </div>

    <template v-else-if="detail">
      <header class="campaign-hero">
        <div class="hero-text">
          <p class="campaign-kicker">{{ t('Kampanya {n}', { n: detail.ordinal }) }}</p>
          <h1 class="hero-title">{{ detail.name }}</h1>
          <div class="meta-row">
            <span class="status-chip" :class="detail.status">
              <span v-if="detail.status === 'sending'" class="live-dot" aria-hidden="true" />{{ statusLabel(detail.status, scheduledAt) }}
            </span>
            <span v-if="content" class="version-tag">v{{ content.version }}</span>
            <span v-if="source" class="source" :class="{ mock: content?.llm_provider === 'mock' }">{{ source }}</span>
            <span v-if="tracked?.approved_at" class="muted">{{ t('onaylandı {when}', { when: relativeTime(tracked.approved_at) }) }}</span>
          </div>
        </div>
        <div class="hero-actions">
          <button v-if="waitsForTime" type="button" class="button" data-test="cancel-scheduled" :disabled="cancelling" @click="askCancel = true">{{ t('Gönderimi iptal et') }}</button>
          <button v-if="!sent && content" type="button" class="button" data-test="edit-campaign" :disabled="saving || refreshPending" @click="startEdit"><Icon name="edit" :size="16" />{{ t('Düzenle') }}</button>
          <button v-if="!sent && content" type="button" class="button primary" data-test="approve-campaign" :disabled="saving || refreshPending || (editing && dirty)" @click="showApprove = true"><Icon name="check" :size="16" />{{ t('Onayla ve gönder') }}</button>
          <RouterLink :to="`/assistant/${detail.chat_id}?campaign=${detail.id}`" class="button" :class="{ primary: !sent }">
            <Icon name="assistant" :size="16" />{{ sent ? t('Asistanda aç') : t('Asistanla geliştir') }}
          </RouterLink>
        </div>
      </header>
      <div v-if="saveNotice" class="alert-strip waiting" role="status">
        {{ saveNotice }}
        <button type="button" class="button" data-test="retry-refresh" @click="refreshSaved">{{ t('Yeniden yükle') }}</button>
      </div>
      <p v-if="cancelError" class="error" role="alert">{{ cancelError }}</p>

      <template v-if="sent">
        <div v-if="waitsForTime && scheduledAt" class="alert-strip waiting" data-test="scheduled-notice" role="status">
          <Icon name="queued" :size="16" />{{ t('Zamanlandı: gönderim {when} tarihinde başlayacak. O zamana kadar hiçbir mesaj gönderilmez; izin her mesajdan hemen önce yeniden kontrol edilir.', { when: dateTimeText(scheduledAt) }) }}
        </div>
        <div v-if="totals" class="kpi-grid">
          <StatTile :label="t('Gönderildi')" icon="sent" tone="good" :value="totals.sent" :split="split('sent')" :info="t('Sağlayıcının kabul ettiği mesajlar.')" :interactive="false" />
          <StatTile
            v-if="waitsForTime"
            :label="t('Zamanlanmış')"
            icon="queued"
            tone="accent"
            :value="totals.queued"
            :split="split('queued')"
            :info="t('Gönderim zamanı gelince kuyruğa girecek mesajlar.')"
            :interactive="false"
          />
          <StatTile v-else :label="t('Kuyrukta')" icon="queued" tone="accent" :value="totals.queued" :split="split('queued')" :info="t('Şu an gönderilmeyi bekleyen mesajlar.')" :interactive="false" />
          <StatTile :label="t('Başarısız')" icon="failed" tone="critical" :value="totals.failed" :split="split('failed')" :info="t('Kesin gönderilemeyen mesajlar.')" :interactive="false" />
          <StatTile
            :label="t('Belirsiz')"
            icon="unknown"
            tone="warning"
            :value="totals.unknown"
            :split="split('unknown')"
            :info="t('Sağlayıcı kabul etmiş olabilir ama yanıt alınamadı; aynı kişiye tekrar gönderilmez.')"
            :interactive="false"
          />
        </div>

        <div class="chart-grid" :class="{ refreshing: analytics.loading.value }">
          <section class="card chart-card">
            <header class="card-head">
              <h2 class="card-title">{{ t('Günlük gönderim') }}</h2>
              <p class="card-sub">{{ t('Bu kampanyanın gönderilen mesajları, ilk gönderimden bugüne') }}</p>
            </header>
            <StackedColumns v-if="data && data.kpis.sent.total" :points="data.series" :bucket="data.window.bucket" />
            <p v-else class="in-frame-empty">{{ data ? t('Henüz gönderilen mesaj yok.') : t('Yükleniyor…') }}</p>
          </section>
          <section class="card chart-card">
            <header class="card-head">
              <h2 class="card-title">{{ t('Şehirlere göre') }}</h2>
              <p class="card-sub">{{ t('Alıcının şehrine göre, ilk {n}', { n: phone ? 5 : 10 }) }}</p>
            </header>
            <CityBars v-if="cities && (cities.top.length || cities.unknown.total)" :cities="cities" />
            <p v-else class="in-frame-empty">{{ t('Şehir dağılımı ilk gönderimlerden sonra oluşur.') }}</p>
          </section>
        </div>
        <AbResults v-if="tracked?.variants" :variants="tracked.variants" />
      </template>
      <div v-else class="alert-strip waiting">
        <Icon name="queued" :size="16" />{{ t('Onay bekliyor: bu kampanya henüz onaylanmadı, gönderim başlamadı. Düzenleyip onaylayabilirsiniz.') }}
      </div>

      <p v-if="editError && !editing" class="error" role="alert">{{ editError }}</p>
      <section v-if="editing && catalog && content && !sent" class="card manual-editor-card">
        <h2 class="card-title">{{ t('Taslağı düzenle') }}</h2>
        <div v-if="staleConflict" class="alert-strip waiting" role="alert">
          {{ t('Başka bir değişiklik kaydedildi. Girdiğiniz alanlar korunuyor. Güncel sürümü yüklemek bu alanları değiştirecek.') }}
          <button type="button" class="button" data-test="reload-conflict" @click="reloadConflict">{{ t('Güncel sürümü yükle') }}</button>
        </div>
        <CampaignEditor :initial="content" :catalog="catalog" :busy="saving" :error="editError" :issues="editIssues" @dirty="dirty = $event" @save="saveEdit" @cancel="editing = false" />
      </section>

      <div v-if="!editing" class="detail-grid">
        <section class="card content-card">
          <header class="card-head">
            <h2 class="card-title">{{ sent ? t('Gönderilen içerik') : t('Taslak içerik') }}</h2>
            <p class="card-sub">{{ sent ? t('Onaylanan metin; her alıcı için bir kez oluşturuldu.') : t('Onaylanınca her alıcı için bir kez oluşturulur.') }}</p>
          </header>
          <LanguagePreview v-if="content" :localization="content.localization ?? null" :summary="content.language_summary ?? null" :sms="content.sms ?? null" :email="content.email ?? null" :ab-test="content.ab_test ?? null" />
        </section>
        <aside class="detail-side">
          <section v-if="approval.length" class="card approval-card">
            <header class="card-head">
              <h2 class="card-title">{{ t('Onayda') }}</h2>
              <p class="card-sub">{{ t('İzin ve içerik tekrarı kontrolü onay anında yapıldı.') }}</p>
            </header>
            <p v-for="a in approval" :key="a.ch" class="approval-line">
              <i class="dot-key" :class="a.ch" aria-hidden="true" /><strong>{{ channelName(a.ch) }}</strong> {{ a.text }}
            </p>
          </section>
          <section v-if="content" class="card audience-card">
            <header class="card-head">
              <h2 class="card-title">{{ t('Hedef kitle') }}</h2>
              <p v-if="content.audience" class="card-sub">{{ t('Taslak anında {n} kişi', { n: formatCount(content.audience.total) }) }}</p>
            </header>
            <ul class="rule-chips">
              <li v-for="(c, i) in content.segment_rule.conditions" :key="i" class="chip" :class="c.kind">{{ conditionText(c) }}</li>
            </ul>
          </section>
        </aside>
      </div>
      <ApproveModal
        v-if="showApprove && detail && !sent"
        :campaign="detail"
        :approve="(versionId: number, options?: ApproveOptions) => api.approve(detail!.id, versionId, options)"
        :preview="(versionId: number) => api.approvalPreview(detail!.id, versionId)"
        :save-decisions="saveDecisions"
        @approved="load"
        @close="showApprove = false"
      />
      <ConfirmDialog
        v-if="askCancel && waitsForTime"
        :title="`${t('Kampanya {n}', { n: detail.ordinal })} · ${detail.name}`"
        :confirm-label="t('Gönderimi iptal et')"
        :busy-label="t('İptal ediliyor…')"
        :busy="cancelling"
        danger
        @confirm="cancelScheduled"
        @close="askCancel = false"
      >
        <p>{{ t('Bu kampanyanın zamanlanmış gönderimini iptal etmek istiyor musunuz?') }}</p>
        <p class="hint">{{ t('Bekleyen mesajlar silinir ve hiçbiri gönderilmez; iptal geri alınamaz.') }}</p>
      </ConfirmDialog>
    </template>
  </section>
</template>
