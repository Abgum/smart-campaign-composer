<script setup lang="ts">
import { t, tk } from '../i18n'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useChatStore } from '../stores/chat'
import { api, ApiError } from '../api/http'
import type { ApproveOptions, Issue, Localization, ManualDraftInput, RuleCatalog, TrackedCampaign } from '../api/types'
import { campaignStatus, channelName, conditionText, formatCount, timeText } from '../lib/format'
import { statusLabel } from '../lib/schedule'
import { useEventStream } from '../lib/stream'
import ApproveModal from './ApproveModal.vue'
import ConfirmDialog from './ConfirmDialog.vue'
import StatusTable from './StatusTable.vue'
import Icon from './Icon.vue'
import LanguagePreview from './assistant/LanguagePreview.vue'
import ApproveBar from './assistant/ApproveBar.vue'
import CampaignEditor from './campaign/CampaignEditor.vue'

const store = useChatStore()
const detail = computed(() => (store.focusId === null ? null : (store.details[store.focusId] ?? null)))
const version = computed(() => detail.value?.current_version ?? null)
const editable = computed(() => detail.value?.status === 'draft' && !store.turn)

const catalog = ref<RuleCatalog | null>(null)
const formError = ref<{ message: string; issues: Issue[] } | null>(null)
const saving = ref(false)
const showApprove = ref(false)
const askCancel = ref(false)
const cancelling = ref(false)
const cancelError = ref('')
const editing = ref(false)
const creating = ref(false)
const dirty = ref(false)

watch(() => version.value?.id, () => {
  formError.value = null
  editing.value = false
  dirty.value = false
})
// The chat holds its message while edits wait to be saved or undone.
watch([editing, creating, dirty], ([open, newCampaign, changed]) => { store.unsavedEdit = (open || newCampaign) && changed }, { immediate: true })
onBeforeUnmount(() => { store.unsavedEdit = false })
async function openEditor(newCampaign = false) {
  formError.value = null
  try {
    catalog.value ??= await api.ruleCatalog()
    if (newCampaign) { creating.value = true; editing.value = false; dirty.value = false }
    else editing.value = true
  } catch { formError.value = { message: t('Kitle kuralı seçenekleri yüklenemedi.'), issues: [] } }
}
async function save(draft: ManualDraftInput) {
  const d = detail.value
  const v = version.value
  if (!d || !v) return
  saving.value = true
  formError.value = null
  try {
    await store.saveEdit(d.id, v.id, draft)
    editing.value = false
    dirty.value = false
  } catch (e) {
    formError.value =
      e instanceof ApiError ? { message: e.message, issues: e.body?.issues ?? [] } : { message: t('Kaydedilemedi.'), issues: [] }
  } finally {
    saving.value = false
  }
}
async function create(draft: ManualDraftInput) {
  saving.value = true
  formError.value = null
  try { await store.createManual(draft); creating.value = false; dirty.value = false }
  catch (e) {
    formError.value = e instanceof ApiError ? { message: e.message, issues: e.body?.issues ?? [] } : { message: t('Kaydedilemedi.'), issues: [] }
  } finally { saving.value = false }
}

// Cancelling asks first in a dialog, which stays open until the request ends.
async function cancelScheduled() {
  const d = detail.value
  if (!d || cancelling.value) return
  cancelling.value = true
  cancelError.value = ''
  try {
    await store.cancelScheduled(d.id)
  } catch (e) {
    cancelError.value = e instanceof ApiError ? e.message : t('Kampanya iptal edilemedi.')
  } finally {
    cancelling.value = false
    askCancel.value = false
  }
}

// A language decision made in the approval dialog is a new version; the
// store reloads the campaign and the dialog shows that version's preview.
async function saveDecisions(versionId: number, localization: Localization) {
  const d = detail.value
  const v = version.value
  if (!d || !v) return
  await store.saveEdit(d.id, versionId, { name: v.name, segment_rule: v.segment_rule, sms: null, email: null, localization })
}

// Progress of an approved campaign, live from its stream.
const tracked = ref<TrackedCampaign | null>(null)
const streamURL = computed(() =>
  detail.value && detail.value.status !== 'draft' ? `/api/campaigns/${detail.value.id}/stream` : null,
)
watch(streamURL, () => (tracked.value = null))
useEventStream(streamURL, (event, data) => {
  if (event === 'campaign' && data.id === detail.value?.id) tracked.value = data
})

// Recipients of the draft who wait for a language decision.
const waiting = computed(() => (version.value?.language_summary?.unresolved ?? []).reduce((n, g) => n + g.count, 0))

const previewSMS = computed(() => version.value?.sms ?? null)
const previewEmail = computed(() => version.value?.email ?? null)

const sourceText = (source: string, provider: string | null, number: number) =>
  source === 'llm' ? (provider === 'mock' ? 'MOCK LLM' : 'Claude') : number === 1 ? t('Manuel olarak oluşturuldu') : t('Manuel olarak düzenlendi')
</script>

<template>
  <section class="campaign-panel" :aria-label="t('Taslak')">
    <nav class="tabs" :aria-label="t('Kampanyalar')">
      <button
        v-for="c in store.campaigns"
        :key="c.id"
        type="button"
        class="tab"
        :class="{ active: c.id === store.focusId }"
        :aria-pressed="c.id === store.focusId"
        @click="store.selectCampaign(c.id === store.focusId ? null : c.id)"
      >
        <span class="tab-title">{{ t('Kampanya {n}', { n: c.ordinal }) }} · <span class="status" :class="c.status">{{ campaignStatus[c.status] }}</span></span>
        <span class="tab-name">{{ c.name }}</span>
      </button>
    </nav>
    <p v-if="store.unsavedEdit" class="muted small" role="status">{{ t('Sekme değiştirmek için önce değişiklikleri kaydedin veya geri alın.') }}</p>

    <div class="manual-chat-action"><button type="button" class="button" data-test="create-manual-in-chat" :disabled="!!store.turn || saving || store.unsavedEdit" @click="openEditor(true)"><Icon name="plus" :size="16" />{{ t('Manuel kampanya oluştur') }}</button></div>

    <div class="campaign-scroll">
      <section v-if="creating && catalog" class="campaign manual-chat-editor">
        <h2>{{ t('Bu sohbete yeni kampanya ekle') }}</h2>
        <p class="muted">{{ t('Bu kampanyayı asistan kullanmadan oluşturabilirsiniz. Daha sonra asistandan geliştirmesini isteyebilirsiniz.') }}</p>
        <CampaignEditor :initial="null" :catalog="catalog" :busy="saving" :error="formError?.message" :issues="formError?.issues" :save-label="tk('Taslağı oluştur')" @dirty="dirty = $event" @save="create" @cancel="creating = false; dirty = false" />
      </section>
      <div v-else-if="!store.campaigns.length" class="canvas-empty">
        <span class="empty-icon" aria-hidden="true"><Icon name="mail" :size="22" /></span>
        <h3>{{ t('Taslak burada görünecek') }}</h3>
        <p>{{ t('İlk mesajınızdan sonra hedef kitle ile SMS ve e-posta önizlemesi bu alanda açılır. İnceleyip düzenleyebilir, hazır olunca onaylarsınız.') }}</p>
      </div>
      <div v-else-if="!detail" class="canvas-empty">
        <span class="empty-icon" aria-hidden="true"><Icon name="campaigns" :size="22" /></span>
        <h3>{{ t('Bir kampanya seçin') }}</h3>
        <p>{{ t('Seçili kampanya burada açılır ve sonraki mesajınızın hedefi olarak önerilir.') }}</p>
      </div>

      <article v-if="!creating && detail && version" class="campaign">
        <header class="campaign-head">
          <p class="campaign-kicker">{{ t('Kampanya {n}', { n: detail.ordinal }) }}</p>
          <h2>{{ detail.name }}</h2>
          <div class="meta-row">
            <span class="status" :class="detail.status">{{ statusLabel(detail.status, detail.scheduled_at) }}</span>
            <span class="version-tag">v{{ version.version }}</span>
            <span class="source" :class="{ mock: version.llm_provider === 'mock' }">{{ sourceText(version.source, version.llm_provider, version.version) }}</span>
            <span v-if="version.change_summary" class="muted">“{{ t(version.change_summary) }}”</span>
          </div>
        </header>

        <section class="canvas-section">
          <h3>{{ t('Hedef kitle') }}</h3>
          <ul class="rule-chips">
            <li v-for="(c, i) in version.segment_rule.conditions" :key="i" class="chip" :class="c.kind">{{ conditionText(c) }}</li>
          </ul>
          <div v-if="version.audience" class="audience-stats">
            <div class="audience-stat">
              <span class="audience-value"><Icon name="users" :size="16" />{{ formatCount(version.audience.total) }}</span>
              <span class="audience-label">{{ t('kişi kitlede') }}</span>
            </div>
            <div class="audience-stat">
              <span class="audience-value"><i class="dot-key sms" aria-hidden="true" />{{ formatCount(version.audience.sms_reachable) }}</span>
              <span class="audience-label">{{ t("SMS'e uygun") }}</span>
            </div>
            <div class="audience-stat">
              <span class="audience-value"><i class="dot-key email" aria-hidden="true" />{{ formatCount(version.audience.email_reachable) }}</span>
              <span class="audience-label">{{ t('e-postaya uygun') }}</span>
            </div>
          </div>
        </section>

        <section class="canvas-section">
          <div class="section-head">
            <h3>{{ t('Önizleme') }}</h3>
            <button v-if="editable && !editing" type="button" class="edit-toggle" @click="openEditor()">
              <Icon name="edit" :size="15" />{{ t('Düzenle') }}
            </button>
          </div>
          <LanguagePreview :localization="version.localization ?? null" :summary="version.language_summary ?? null" :sms="previewSMS" :email="previewEmail" :ab-test="version.ab_test ?? null" />
        </section>

        <section v-if="editable && editing && catalog" class="canvas-section content-editor">
          <h3>{{ t('Düzenle') }}</h3>
          <CampaignEditor :initial="version" :catalog="catalog" :busy="saving" :error="formError?.message" :issues="formError?.issues" @dirty="dirty = $event" @save="save" @cancel="editing = false; dirty = false" />
        </section>

        <section v-if="detail.status !== 'draft'" class="canvas-section progress">
          <h3>{{ t('Gönderim') }}</h3>
          <div v-if="detail.approval" class="approval-summary">
            <span v-for="(c, ch) in detail.approval" :key="ch">
              <i class="dot-key" :class="String(ch)" aria-hidden="true" /><strong>{{ channelName(String(ch)) }}:</strong>
              {{ t('{selected} kişi seçildi · {consent} izin yok · {duplicate} içerik tekrarı', { selected: formatCount(c.selected), consent: formatCount(c.excluded_consent), duplicate: formatCount(c.excluded_duplicate) }) }}
            </span>
          </div>
          <StatusTable v-if="tracked" :channels="tracked.channels" />
          <p v-else class="muted">{{ t('Durum yükleniyor…') }}</p>
        </section>

        <details class="canvas-section versions">
          <summary>{{ t('Versiyonlar ({n})', { n: detail.versions.length }) }}</summary>
          <ol>
            <li v-for="v in detail.versions" :key="v.id" :class="{ current: v.id === version.id }">
              <span class="v">v{{ v.version }}</span>
              <span class="source" :class="{ mock: v.llm_provider === 'mock' }">{{ sourceText(v.source, v.llm_provider, v.version) }}</span>
              <span>{{ t(v.change_summary) }}</span>
              <span class="muted">{{ timeText(v.created_at) }}</span>
            </li>
          </ol>
        </details>
      </article>
    </div>

    <p v-if="cancelError" class="error" role="alert">{{ cancelError }}</p>
    <ApproveBar
      v-if="!creating && detail && version"
      :status="detail.status"
      :scheduled-at="detail.scheduled_at"
      :dirty="dirty"
      :busy="saving || cancelling || !!store.turn"
      :audience="version.audience ?? null"
      :waiting="waiting"
      @approve="showApprove = true"
      @cancel="askCancel = true"
    />

    <ConfirmDialog
      v-if="askCancel && detail"
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

    <ApproveModal
      v-if="showApprove && detail"
      :campaign="detail"
      :approve="(versionId: number, options?: ApproveOptions) => store.approve(detail!.id, versionId, options)"
      :preview="(versionId: number) => api.approvalPreview(detail!.id, versionId)"
      :save-decisions="saveDecisions"
      @close="showApprove = false"
    />
  </section>
</template>
