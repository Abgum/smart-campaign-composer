<script setup lang="ts">
import { t } from '../i18n'
import { computed, ref, watch } from 'vue'
import { ApiError } from '../api/http'
import type { ApprovalPreview, ApproveOptions, ApproveResult, CampaignDetail, Issue, LanguageCounts, LanguageDecision, Localization } from '../api/types'
import { channelName, formatCount } from '../lib/format'
import { countryName, groupLabel, languageName, orderLanguages } from '../lib/languages'
import { useDialogFocus } from '../lib/dialog'
import { dateTimeText, localInputValue, MAX_AHEAD_MS, toUTC, zoneName } from '../lib/schedule'

// Approval renders the content for every recipient once and queues it; the
// consent and duplicate-content filters run then, so their final counts come
// back with the answer. With a preview the dialog first shows what approving
// would do now (nothing is written): recipients per language and channel,
// who is left out and why, the country mapping and the decisions, and each
// message as a recipient gets it. Recipients waiting for a language decision
// lock the approval; a decision made here is saved as a new version (the
// parent reloads the campaign) and the preview follows that version.
// Sending starts at approval unless the marketer picks a later moment: the
// text is rendered and the recipients chosen now either way, and consent is
// checked again right before each message.
const props = defineProps<{
  campaign: CampaignDetail
  approve: (versionId: number, options?: ApproveOptions) => Promise<ApproveResult>
  preview?: (versionId: number) => Promise<ApprovalPreview>
  saveDecisions?: (versionId: number, localization: Localization) => Promise<void>
}>()
const emit = defineEmits<{ close: []; approved: [result: ApproveResult] }>()

const state = ref<'confirm' | 'sending' | 'done' | 'error'>('confirm')
// A modal for the keyboard too: focus starts on "Vazgeç", stays inside, Esc
// closes, and focus moves to the outcome once there is one.
const box = ref<HTMLElement | null>(null)
const { onKey, focusFirst } = useDialogFocus(box, () => emit('close'))
watch(state, (s) => s !== 'sending' && void focusFirst())
const result = ref<ApproveResult | null>(null)
const failure = ref<{ message: string; issues: Issue[] }>({ message: '', issues: [] })

const version = computed(() => props.campaign.current_version)
const isMock = computed(() => version.value?.llm_provider === 'mock')
const source = computed(() => {
  const v = version.value
  if (!v) return ''
  if (v.source !== 'llm') return v.version === 1 ? t('Manuel olarak oluşturuldu') : t('Manuel olarak düzenlendi')
  return v.llm_provider === 'mock' ? t("MOCK LLM ile üretildi (Claude'a bağlı değil)") : t('Claude ile üretildi')
})

// ---- what approving would do now ----
const plan = ref<ApprovalPreview | null>(null)
const planError = ref<{ message: string; issues: Issue[] } | null>(null)
const loading = ref(false)
const notice = ref('') // why the last approval did not start
const choices = ref<Record<string, string>>({})

async function loadPreview() {
  const v = version.value
  if (!props.preview || !v) return
  loading.value = true
  try {
    const p = await props.preview(v.id)
    if (version.value?.id !== v.id) return // a newer version is loading
    plan.value = p
    planError.value = null
    choices.value = Object.fromEntries((p.unresolved ?? []).map((g) => [g.group, '']))
  } catch (e) {
    plan.value = null
    planError.value =
      e instanceof ApiError ? { message: e.message, issues: e.body?.issues ?? [] } : { message: t('Önizleme yüklenemedi.'), issues: [] }
  } finally {
    loading.value = false
  }
}
watch(
  () => version.value?.id,
  () => {
    notice.value = ''
    void loadPreview()
  },
  { immediate: true },
)

const channels = (counts: LanguageCounts) =>
  ['sms', 'email'].filter((ch) => Object.values(counts).some((byChannel) => byChannel[ch]))
const languages = (counts: LanguageCounts) => orderLanguages(Object.keys(counts))
// An A/B test's arms: the messages each queues per channel, and those it
// leaves out because the person already got (or will get) that text.
const arms = ['A', 'B']
const duplicates = (byChannel: Record<string, { excluded_duplicate: number }> | undefined) =>
  Object.values(byChannel ?? {}).reduce((n, c) => n + c.excluded_duplicate, 0)
const waiting = computed(() => plan.value?.unresolved ?? [])
const waitingTotal = computed(() => waiting.value.reduce((n, g) => n + g.count, 0))
const issues = computed(() => plan.value?.issues ?? [])

// ---- when sending starts ----
const when = ref<'now' | 'later'>('now')
const sendAt = ref('') // date-time input value, local time
const earliest = localInputValue(new Date())
const sendAtUTC = computed(() => (when.value === 'later' ? toUTC(sendAt.value) : null))
const scheduleProblem = computed(() => {
  if (when.value !== 'later') return ''
  if (!sendAt.value) return t('Bir tarih ve saat seçin.')
  const at = sendAtUTC.value ? Date.parse(sendAtUTC.value) : NaN
  if (isNaN(at)) return t('Tarih ve saat okunamadı.')
  if (at <= Date.now()) return t('Gönderim zamanı gelecekte olmalı.')
  if (at > Date.now() + MAX_AHEAD_MS) return t('Gönderim zamanı en fazla bir yıl sonrası olabilir.')
  return ''
})

const blocked = computed(
  () =>
    (!!props.preview && (loading.value || !plan.value || waiting.value.length > 0 || issues.value.length > 0)) ||
    !!scheduleProblem.value,
)

// ---- decisions on waiting recipients ----
const variantLanguages = computed(() => version.value?.localization?.variants?.map((v) => v.language) ?? [])
const savingDecisions = ref(false)
const canDecide = computed(() => !!props.saveDecisions && waiting.value.some((g) => !!choices.value[g.group]))
async function decide() {
  const v = version.value
  const loc = v?.localization
  if (!props.saveDecisions || !v || !loc || !canDecide.value) return
  const added: LanguageDecision[] = waiting.value
    .filter((g) => !!choices.value[g.group])
    .map((g) =>
      choices.value[g.group] === 'exclude'
        ? { group: g.group, action: 'exclude' }
        : { group: g.group, action: 'assign', language: choices.value[g.group] },
    )
  savingDecisions.value = true
  try {
    await props.saveDecisions(v.id, { ...loc, decisions: [...(loc.decisions ?? []), ...added] })
  } catch (e) {
    planError.value =
      e instanceof ApiError ? { message: e.message, issues: e.body?.issues ?? [] } : { message: t('Kararlar kaydedilemedi.'), issues: [] }
  } finally {
    savingDecisions.value = false
  }
}

async function confirm() {
  if (!version.value || blocked.value) return
  state.value = 'sending'
  try {
    result.value = sendAtUTC.value
      ? await props.approve(version.value.id, { scheduled_at: sendAtUTC.value })
      : await props.approve(version.value.id)
    state.value = 'done'
    emit('approved', result.value)
  } catch (e) {
    // The audience is worked out again at approval: recipients who joined
    // since the preview and wait for a decision stop it before anything is
    // queued. Show them and ask.
    if (e instanceof ApiError && e.code === 'language_decision_required' && props.preview) {
      notice.value = e.message
      state.value = 'confirm'
      await loadPreview()
      return
    }
    failure.value =
      e instanceof ApiError
        ? { message: e.message, issues: e.body?.issues ?? [] }
        : { message: t('Onay gönderilemedi; bağlantınızı kontrol edin.'), issues: [] }
    state.value = 'error'
  }
}

// What changed between the preview and the approval itself.
const changes = computed(() => {
  const before = plan.value?.languages
  const after = result.value?.languages
  if (!before || !after || result.value?.already_approved) return []
  const out: string[] = []
  for (const lang of orderLanguages([...new Set([...Object.keys(before), ...Object.keys(after)])])) {
    for (const ch of ['sms', 'email']) {
      const was = before[lang]?.[ch]?.selected ?? 0
      const is = after[lang]?.[ch]?.selected ?? 0
      if (was !== is) {
        out.push(t('{language} · {channel}: {before} → {after}', {
          language: languageName(lang), channel: channelName(ch), before: formatCount(was), after: formatCount(is),
        }))
      }
    }
  }
  return out
})
</script>

<template>
  <div class="modal-backdrop" @click.self="emit('close')">
    <div ref="box" class="modal" :class="{ wide: !!preview }" role="dialog" aria-modal="true" aria-labelledby="approve-title" tabindex="-1" @keydown="onKey">
      <h2 id="approve-title">{{ t('Kampanya {n}', { n: campaign.ordinal }) }} · {{ campaign.name }}</h2>

      <template v-if="state === 'confirm' || state === 'sending'">
        <p>
          <strong>v{{ version?.version }}</strong> {{ t('onaylanacak. Onaydan sonra kampanya değiştirilemez; metin her alıcı için bir kez oluşturulur ve gönderilen metin tam olarak odur.') }}
        </p>
        <dl class="facts">
          <dt>{{ t('Taslak kaynağı') }}</dt>
          <dd :class="{ mock: isMock }">{{ source }}</dd>
          <template v-if="version?.audience && !preview">
            <dt>{{ t('Kitle (taslak anında)') }}</dt>
            <dd>
              {{ t("{total} kişi · SMS'e uygun {sms} · e-postaya uygun {email}", { total: version.audience.total, sms: version.audience.sms_reachable, email: version.audience.email_reachable }) }}
            </dd>
          </template>
        </dl>

        <p v-if="notice" class="error" role="alert">{{ notice }}</p>
        <p v-if="preview && loading && !plan" class="muted">{{ t('Önizleme hazırlanıyor…') }}</p>
        <div v-if="planError" class="error" role="alert">
          {{ planError.message }}
          <ul v-if="planError.issues.length" class="issues">
            <li v-for="(is, i) in planError.issues" :key="i"><code>{{ is.path }}</code> {{ is.message }}</li>
          </ul>
        </div>

        <template v-if="plan">
          <table class="language-table">
            <caption>{{ t('Dil ve kanala göre alıcılar (şu anki kitle)') }}</caption>
            <thead>
              <tr>
                <th scope="col">{{ t('Dil') }}</th>
                <th v-for="ch in channels(plan.languages)" :key="ch" scope="col">{{ channelName(ch) }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="lang in languages(plan.languages)" :key="lang">
                <th scope="row">{{ languageName(lang) }}</th>
                <td v-for="ch in channels(plan.languages)" :key="ch">{{ formatCount(plan.languages[lang]?.[ch]?.selected ?? 0) }}</td>
              </tr>
            </tbody>
          </table>

          <table v-if="plan.variants" class="language-table ab-table">
            <caption>{{ t('A/B varyantına göre mesajlar (şu anki kitle)') }}</caption>
            <thead>
              <tr>
                <th scope="col">{{ t('Varyant') }}</th>
                <th v-for="ch in channels(plan.variants)" :key="ch" scope="col">{{ channelName(ch) }}</th>
                <th scope="col">{{ t('İçerik tekrarı') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="a in arms" :key="a">
                <th scope="row">{{ a }}</th>
                <td v-for="ch in channels(plan.variants)" :key="ch">{{ formatCount(plan.variants[a]?.[ch]?.selected ?? 0) }}</td>
                <td>{{ formatCount(duplicates(plan.variants[a])) }}</td>
              </tr>
            </tbody>
          </table>
          <p v-if="plan.variants" class="hint">{{ t('Her alıcı tek varyant alır, iki kanalda da aynısını; atama kampanya ve kişiye göre sabittir.') }}</p>

          <ul class="language-excluded">
            <li v-for="g in plan.excluded_language ?? []" :key="g.group">
              {{ t('Dil kararıyla gönderilmeyecek:') }} {{ groupLabel(g.group) }}: {{ formatCount(g.count) }}
            </li>
            <template v-for="(c, ch) in plan.channels" :key="ch">
              <li v-if="c.excluded_consent">{{ channelName(String(ch)) }}: {{ t('{n} kişi izin ya da iletişim bilgisi olmadığı için elendi.', { n: formatCount(c.excluded_consent) }) }}</li>
              <li v-if="c.excluded_duplicate" class="duplicate">{{ channelName(String(ch)) }}: {{ t('{n} kişi bu içeriği zaten aldı / alacak; gönderilmeyecek.', { n: formatCount(c.excluded_duplicate) }) }}</li>
            </template>
          </ul>

          <div v-if="plan.country_languages?.length || plan.decisions?.length" class="language-mapping">
            <h3>{{ t('Dil eşlemesi') }}</h3>
            <p class="hint">{{ t('Kayıtlı tercihi desteklenen alıcıya her zaman kendi dili gider. Tercihi olmayanlar için ülke eşlemesini taslağı yazan model önerdi.') }}</p>
            <ul>
              <li v-for="c in plan.country_languages ?? []" :key="c.country">
                {{ countryName(c.country) }} → {{ languageName(c.language) }}
              </li>
              <li v-for="d in plan.decisions ?? []" :key="d.group">
                {{ groupLabel(d.group) }} → {{ d.action === 'exclude' ? t('Gönderme') : languageName(d.language ?? '') }}
              </li>
            </ul>
          </div>

          <div v-if="waiting.length" class="language-decisions">
            <p class="decisions-title">
              <strong>{{ t('{n} alıcı dil kararı bekliyor; karar verilmeden gönderim başlamaz.', { n: formatCount(waitingTotal) }) }}</strong>
            </p>
            <div v-for="g in waiting" :key="g.group" class="decision-row">
              <label :for="'decision-' + g.group">{{ groupLabel(g.group) }}: {{ formatCount(g.count) }}</label>
              <select :id="'decision-' + g.group" v-model="choices[g.group]" :data-group="g.group" :disabled="!saveDecisions || savingDecisions">
                <option value="">{{ t('Seçin…') }}</option>
                <option v-for="l in variantLanguages" :key="l" :value="l">{{ languageName(l) }}</option>
                <option value="exclude">{{ t('Gönderme') }}</option>
              </select>
              <p v-if="g.group.startsWith('variant:')" class="hint">{{ t('Bu dilde varyant yok; asistandan bu dilde bir varyant isteyebilirsiniz.') }}</p>
            </div>
            <p class="hint">{{ t('Kararlar yeni bir versiyon olarak kaydedilir; ardından onay yeniden istenir.') }}</p>
            <button type="button" class="save-decisions" :disabled="!canDecide || savingDecisions" @click="decide">
              {{ savingDecisions ? t('Kaydediliyor…') : t('Kararı kaydet') }}
            </button>
          </div>

          <ul v-if="issues.length" class="issues">
            <li v-for="(is, i) in issues" :key="i"><code>{{ is.path }}</code> {{ is.message }}</li>
          </ul>

          <details v-if="plan.samples?.length" class="language-samples">
            <summary>{{ t('Mesaj önizlemeleri ({n})', { n: plan.samples.length }) }}</summary>
            <p class="hint">{{ t('Her dil ve kanal için ilk alıcıya gidecek metin.') }}</p>
            <article v-for="s in plan.samples" :key="s.language + s.channel + (s.variant ?? '')" class="sample">
              <h4>{{ languageName(s.language) }} · {{ channelName(s.channel) }}<template v-if="s.variant"> · {{ s.variant }}</template></h4>
              <p v-if="s.subject" class="sample-subject">{{ s.subject }}</p>
              <p class="sample-body">{{ s.body }}</p>
            </article>
          </details>
        </template>

        <fieldset class="send-when">
          <legend>{{ t('Gönderim zamanı') }}</legend>
          <label><input v-model="when" type="radio" name="send-when" value="now" :disabled="state === 'sending'" /> {{ t('Hemen gönder') }}</label>
          <label><input v-model="when" type="radio" name="send-when" value="later" :disabled="state === 'sending'" /> {{ t('İleri bir tarih ve saatte') }}</label>
          <template v-if="when === 'later'">
            <label for="send-at" class="send-at-label">{{ t('Tarih ve saat') }}</label>
            <input id="send-at" v-model="sendAt" type="datetime-local" :min="earliest" :disabled="state === 'sending'" aria-describedby="send-at-hint" />
            <p id="send-at-hint" class="hint">{{ t('Saat bu cihazın saat diliminde ({zone}) girilir; UTC olarak saklanır.', { zone: zoneName() }) }}</p>
            <p v-if="scheduleProblem" class="error" role="alert">{{ scheduleProblem }}</p>
          </template>
        </fieldset>

        <p class="hint">
          {{ t('İzin ve içerik tekrarı kontrolü onay anında yapılır: aynı kişiye aynı kanaldan aynı metin, kampanyadan bağımsız olarak bir kez gider.') }}
        </p>
        <div class="actions">
          <button type="button" data-autofocus @click="emit('close')">{{ t('Vazgeç') }}</button>
          <button type="button" class="confirm primary" :disabled="state === 'sending' || blocked" @click="confirm">
            {{ state === 'sending' ? t('Onaylanıyor…') : when === 'later' ? t('Onayla ve zamanla') : t('Onayla ve kuyruğa al') }}
          </button>
        </div>
      </template>

      <template v-else-if="state === 'done' && result">
        <p v-if="result.already_approved">{{ t('Bu versiyon zaten onaylanmıştı.') }}</p>
        <template v-else-if="result.scheduled_at">
          <p>{{ t('Onaylandı; gönderim {when} tarihinde başlayacak.', { when: dateTimeText(result.scheduled_at) }) }}</p>
          <p class="hint">{{ t('Metin şimdi oluşturuldu ve değişmeyecek; izin her mesajdan hemen önce yeniden kontrol edilir.') }}</p>
        </template>
        <p v-else>{{ t('Onaylandı; gönderim kuyruğa alındı.') }}</p>
        <div v-for="(c, ch) in result.channels" :key="ch" class="channel-result">
          <h3>{{ channelName(String(ch)) }}</h3>
          <ul>
            <li>
              <strong>{{ c.selected }}</strong> {{ t('kişiye gönderilecek.') }}
            </li>
            <li v-if="c.excluded_consent">{{ t('{n} kişi izin ya da iletişim bilgisi olmadığı için elendi.', { n: c.excluded_consent }) }}</li>
            <li v-if="c.excluded_duplicate" class="duplicate">
              {{ t('{n} kişi bu içeriği zaten aldı / alacak; gönderilmeyecek.', { n: c.excluded_duplicate }) }}
            </li>
          </ul>
        </div>
        <table v-if="result.languages && Object.keys(result.languages).length" class="language-table">
          <caption>{{ t('Dil ve kanala göre kuyruğa alınanlar') }}</caption>
          <thead>
            <tr>
              <th scope="col">{{ t('Dil') }}</th>
              <th v-for="ch in channels(result.languages)" :key="ch" scope="col">{{ channelName(ch) }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="lang in languages(result.languages)" :key="lang">
              <th scope="row">{{ languageName(lang) }}</th>
              <td v-for="ch in channels(result.languages)" :key="ch">{{ formatCount(result.languages[lang]?.[ch]?.selected ?? 0) }}</td>
            </tr>
          </tbody>
        </table>
        <table v-if="result.variants" class="language-table ab-table">
          <caption>{{ t('A/B varyantına göre kuyruğa alınanlar') }}</caption>
          <thead>
            <tr>
              <th scope="col">{{ t('Varyant') }}</th>
              <th v-for="ch in channels(result.variants)" :key="ch" scope="col">{{ channelName(ch) }}</th>
              <th scope="col">{{ t('İçerik tekrarı') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="a in arms" :key="a">
              <th scope="row">{{ a }}</th>
              <td v-for="ch in channels(result.variants)" :key="ch">{{ formatCount(result.variants[a]?.[ch]?.selected ?? 0) }}</td>
              <td>{{ formatCount(duplicates(result.variants[a])) }}</td>
            </tr>
          </tbody>
        </table>
        <div v-if="changes.length" class="language-diff">
          <h3>{{ t('Önizlemeden bu yana değişen sayılar') }}</h3>
          <p class="hint">{{ t('Kitle onay anında yeniden hesaplandı; yeni karar bekleyen alıcı çıkmadığı için gönderim başladı.') }}</p>
          <ul>
            <li v-for="c in changes" :key="c">{{ c }}</li>
          </ul>
        </div>
        <div class="actions">
          <button type="button" class="primary" @click="emit('close')">{{ t('Tamam') }}</button>
        </div>
      </template>

      <template v-else-if="state === 'error'">
        <p class="error">{{ failure.message }}</p>
        <ul v-if="failure.issues.length" class="issues">
          <li v-for="(is, i) in failure.issues" :key="i">
            <code>{{ is.path }}</code> {{ is.message }}
          </li>
        </ul>
        <div class="actions">
          <button type="button" @click="emit('close')">{{ t('Kapat') }}</button>
        </div>
      </template>
    </div>
  </div>
</template>
