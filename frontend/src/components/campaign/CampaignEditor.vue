<script setup lang="ts">
import { t, tk } from '../../i18n'
import { computed, ref, watch } from 'vue'
import type { Issue, ManualDraftInput, RuleCatalog, SegmentRule, Version } from '../../api/types'
import { formatCount, templateLength } from '../../lib/format'
import { contentLanguages, countryName, languageName, orderLanguages } from '../../lib/languages'
import DraftPreview from '../assistant/DraftPreview.vue'
import RuleBuilder from './RuleBuilder.vue'

const props = withDefaults(defineProps<{
  initial: Version | null
  catalog: RuleCatalog
  busy?: boolean
  error?: string
  issues?: Issue[]
  saveLabel?: string
}>(), { busy: false, error: '', issues: () => [], saveLabel: tk('Yeni versiyon olarak kaydet') })
const emit = defineEmits<{ save: [draft: ManualDraftInput]; cancel: []; dirty: [value: boolean] }>()

// A draft in each recipient's language is edited variant by variant; its
// channels are the same in every language. Its country mapping (the language
// of those without a supported preference, by country) is edited here too;
// its decisions are kept as they are. One content for everyone states its
// language ("" for a version from before there were languages, which keeps
// it unstated). An A/B test gives every content a B, edited apart on the
// same channels: bTexts runs parallel to texts.
type Texts = { language: string; sms: string; subject: string; body: string }
type Mapping = { country: string; language: string }
type Form = {
  name: string; segment_rule: SegmentRule; smsOn: boolean; emailOn: boolean
  language: string; texts: Texts[]; countries: Mapping[]; abOn: boolean; bTexts: Texts[]
}
const perRecipient = computed(() => props.initial?.localization?.mode === 'per_recipient')
function fromVersion(v: Version | null): Form {
  const variants = v?.localization?.mode === 'per_recipient' ? (v.localization.variants ?? []) : null
  const texts: Texts[] = variants
    ? variants.map((x) => ({ language: x.language, sms: x.sms?.text ?? '', subject: x.email?.subject ?? '', body: x.email?.body ?? '' }))
    : [{ language: '', sms: v?.sms?.text ?? '', subject: v?.email?.subject ?? '', body: v?.email?.body ?? '' }]
  const bs = v?.ab_test?.variants ?? null
  // A B starts as a copy of its A until it is written.
  const bTexts = texts.map((x) => {
    const b = variants ? bs?.find((y) => y.language === x.language) : bs?.[0]
    return b ? { language: x.language, sms: b.sms?.text ?? '', subject: b.email?.subject ?? '', body: b.email?.body ?? '' } : { ...x }
  })
  return {
    name: v?.name ?? '',
    segment_rule: v ? JSON.parse(JSON.stringify(v.segment_rule)) as SegmentRule : { match: 'all', conditions: [] },
    smsOn: variants ? !!variants[0]?.sms : !!v?.sms,
    emailOn: variants ? !!variants[0]?.email : !!v?.email,
    language: v ? (v.localization?.mode === 'single' ? (v.localization.language ?? '') : '') : 'tr',
    texts,
    countries: variants ? (v?.localization?.country_languages ?? []).map((c) => ({ country: c.country, language: c.language })) : [],
    abOn: !!bs,
    bTexts,
  }
}
const form = ref<Form>(fromVersion(props.initial))
const active = ref(0)
const arm = ref<'A' | 'B'>('A')
watch(() => props.initial?.id, () => { form.value = fromVersion(props.initial); active.value = 0; arm.value = 'A' })
// Turning the test on starts each B from its A as it stands now.
watch(() => form.value.abOn, (on) => {
  if (on && !props.initial?.ab_test) form.value.bTexts = form.value.texts.map((x) => ({ ...x }))
  if (!on) arm.value = 'A'
})
const dirty = computed(() => JSON.stringify(form.value) !== JSON.stringify(fromVersion(props.initial)))
watch(dirty, (value) => emit('dirty', value), { immediate: true })
const shownTexts = computed(() => (form.value.abOn && arm.value === 'B' ? form.value.bTexts : form.value.texts))
const current = computed(() => shownTexts.value[active.value] ?? shownTexts.value[0])
const smsOf = (x: Texts) => (form.value.smsOn ? { text: x.sms } : null)
const emailOf = (x: Texts) => (form.value.emailOn ? { subject: x.subject, body: x.body } : null)
const textsOK = (x: Texts) =>
  (!form.value.smsOn || (!!x.sms.trim() && templateLength(x.sms) <= 160)) &&
  (!form.value.emailOn || (!!x.subject.trim() && !!x.body.trim() && templateLength(x.subject) <= 80))
const allOK = (list: Texts[]) => list.every(textsOK)
// A B the API would refuse: it sends what its A sends (spaces aside).
const same = (a: string, b: string) => a.normalize('NFC').trim().replace(/\s+/g, ' ') === b.normalize('NFC').trim().replace(/\s+/g, ' ')
const sameAsA = computed(() =>
  form.value.abOn
    ? form.value.texts.filter((a, i) => {
        const b = form.value.bTexts[i]
        return (!form.value.smsOn || same(a.sms, b.sms)) && (!form.value.emailOn || (same(a.subject, b.subject) && same(a.body, b.body)))
      })
    : [],
)
const sameLabel = computed(() => sameAsA.value.map((x) => (x.language ? languageName(x.language) : t('ortak içerik'))).join(', '))
// The language split of the saved version (as of the draft: the recipients
// it can message, each once), first in short; the country mapping sits
// under advanced settings, each line with the recipients it decides (those
// there without a supported preference).
const summary = computed(() => props.initial?.language_summary ?? null)
const distribution = computed(() => {
  const langs = summary.value?.languages ?? {}
  return orderLanguages(Object.keys(langs)).map((l) => `${languageName(l)} ${formatCount(langs[l].total)}`).join(' · ')
})
const waitingTotal = computed(() => (summary.value?.unresolved ?? []).reduce((n, g) => n + g.count, 0))
// A version saved before the counts were kept has none: no number then.
const counted = computed(() => summary.value?.countries != null)
const decided = (country: string) => summary.value?.countries?.[country] ?? 0

// The countries of the place catalog; a row offers those no other row maps.
const catalogCountries = computed(() =>
  props.catalog.places.filter((p) => p.level === 'country' && p.code).map((p) => p.code as string),
)
const freeCountries = computed(() => catalogCountries.value.filter((c) => !form.value.countries.some((m) => m.country === c)))
function countryOptions(i: number): string[] {
  const own = form.value.countries[i].country
  return [...new Set([own, ...catalogCountries.value])].filter((c) => c === own || freeCountries.value.includes(c))
    .sort((a, b) => catalogCountries.value.indexOf(a) - catalogCountries.value.indexOf(b))
}
function addCountry() {
  const country = freeCountries.value[0]
  if (country) form.value.countries.push({ country, language: form.value.texts[0]?.language ?? 'tr' })
}

const canSave = computed(() =>
  dirty.value && !!form.value.name.trim() && form.value.segment_rule.conditions.length > 0 &&
  (form.value.smsOn || form.value.emailOn) && allOK(form.value.texts) &&
  (!form.value.abOn || (allOK(form.value.bTexts) && sameAsA.value.length === 0)),
)
function save() {
  if (!canSave.value || props.busy) return
  const draft: ManualDraftInput = {
    name: form.value.name.trim(),
    segment_rule: JSON.parse(JSON.stringify(form.value.segment_rule)) as SegmentRule,
    sms: null,
    email: null,
  }
  if (perRecipient.value) {
    draft.localization = {
      ...props.initial!.localization!,
      variants: form.value.texts.map((x) => ({ language: x.language, sms: smsOf(x), email: emailOf(x) })),
      country_languages: form.value.countries.map((c) => ({ country: c.country, language: c.language })),
    }
  } else {
    draft.sms = smsOf(form.value.texts[0])
    draft.email = emailOf(form.value.texts[0])
    if (form.value.language) draft.localization = { mode: 'single', language: form.value.language }
  }
  // Each B in its A's language: a variant's, or the one content's.
  draft.ab_test = form.value.abOn
    ? {
        variants: form.value.bTexts.map((x, i) => ({
          language: perRecipient.value ? form.value.texts[i].language : form.value.language,
          sms: smsOf(x),
          email: emailOf(x),
        })),
      }
    : null
  emit('save', draft)
}
function cancel() {
  form.value = fromVersion(props.initial)
  emit('cancel')
}
</script>

<template>
  <form class="campaign-editor" @submit.prevent="save">
    <label class="field">{{ t('Kampanya adı') }}
      <input v-model="form.name" data-test="campaign-name" maxlength="120" :disabled="busy" required :placeholder="t('Örn. Kasım indirimi')" />
    </label>

    <RuleBuilder v-model="form.segment_rule" :catalog="catalog" />

    <fieldset class="editor-channel">
      <legend>{{ t('Kanallar ve mesajlar') }}</legend>
      <label v-if="!perRecipient" class="field">{{ t('İçerik dili') }}
        <select v-model="form.language" data-test="content-language" :disabled="busy">
          <option v-if="!form.language" value="">{{ t('Belirtilmemiş') }}</option>
          <option v-for="l in contentLanguages" :key="l" :value="l">{{ languageName(l) }}</option>
        </select>
      </label>
      <template v-if="perRecipient">
        <p class="muted small">{{ t('Her alıcıya kendi dilindeki varyant gider. Varyantları tek tek düzenleyin; kanallar her dilde aynıdır.') }}</p>
        <div class="variant-tabs" role="tablist" :aria-label="t('Dil varyantları')">
          <button
            v-for="(x, i) in form.texts"
            :key="x.language"
            type="button"
            role="tab"
            :aria-selected="i === active"
            :class="{ active: i === active, invalid: !textsOK(x) || (form.abOn && !textsOK(form.bTexts[i])) }"
            @click="active = i"
          >{{ languageName(x.language) }}</button>
        </div>
      </template>
      <label class="toggle"><input v-model="form.abOn" data-test="ab-on" type="checkbox" :disabled="busy" /> {{ t('A/B testi: iki mesaj seçeneği') }}</label>
      <template v-if="form.abOn">
        <p class="muted small">{{ t("Alıcıların yaklaşık yarısı A'yı, yarısı B'yi alır; bir alıcı iki kanalda da aynı varyantı alır. Her içeriğin A ve B metni ayrı yazılır.") }}</p>
        <div class="arm-tabs" role="tablist" :aria-label="t('A/B varyantları')">
          <button
            v-for="x in (['A', 'B'] as const)"
            :key="x"
            type="button"
            role="tab"
            :data-test="'arm-' + x"
            :aria-selected="arm === x"
            :class="{ active: arm === x, invalid: !allOK(x === 'A' ? form.texts : form.bTexts) }"
            @click="arm = x"
          >{{ t('Varyant {arm}', { arm: x }) }}</button>
        </div>
        <p v-if="sameAsA.length" class="error small" role="alert">{{ t('B, A ile aynı ({list}); en az bir metni değiştirin.', { list: sameLabel }) }}</p>
      </template>
      <label class="toggle"><input v-model="form.smsOn" data-test="sms-on" type="checkbox" :disabled="busy" /> {{ t('SMS gönder') }}</label>
      <template v-if="form.smsOn">
        <label class="field">{{ t('SMS metni') }}<textarea v-model="current.sms" data-test="sms-text" rows="3" :disabled="busy" /></label>
        <div class="counter" :class="{ over: templateLength(current.sms) > 160 }">{{ t('{n}/160 karakter', { n: templateLength(current.sms) }) }}</div>
      </template>
      <label class="toggle"><input v-model="form.emailOn" data-test="email-on" type="checkbox" :disabled="busy" /> {{ t('E-posta gönder') }}</label>
      <template v-if="form.emailOn">
        <label class="field">{{ t('E-posta konusu') }}<input v-model="current.subject" data-test="email-subject" :disabled="busy" /></label>
        <div class="counter" :class="{ over: templateLength(current.subject) > 80 }">{{ t('{n}/80 karakter', { n: templateLength(current.subject) }) }}</div>
        <label class="field">{{ t('E-posta gövdesi') }}<textarea v-model="current.body" data-test="email-body" rows="6" :disabled="busy" /></label>
      </template>
      <p class="muted small">{{ t('Kişiselleştirme:') }} <code v-pre>{{first_name}}</code>, <code v-pre>{{last_name}}</code>, <code v-pre>{{city}}</code>.</p>
      <p class="muted small">{{ t('Sayaçta her değişken 10 karakter sayılır; kesin uzunluk onayda her alıcı için yeniden ölçülür.') }}</p>
    </fieldset>

    <fieldset v-if="perRecipient" class="editor-languages">
      <legend>{{ t('Diller') }}</legend>
      <p v-if="distribution" class="language-distribution">{{ t('Dil dağılımı: {list}', { list: distribution }) }}</p>
      <p v-if="distribution" class="muted small language-scope">
        {{ t('Sayılar son kaydedilen sürüme aittir; bu ekrandaki değişiklikler kaydedilene kadar güncellenmez.') }}
        {{ t('Sayılar taslak anına ait, onayda yeniden hesaplanır: kampanyanın açık kanallarından en az birinden mesaj alabilecek alıcılar; iki kanala da uygun olan bir kez sayılır.') }}
      </p>
      <p v-if="waitingTotal" class="language-waiting-line">
        {{ t('{n} alıcı dil kararı bekliyor; karar verilene kadar onay kilitli.', { n: formatCount(waitingTotal) }) }}
      </p>
      <details class="language-advanced">
        <summary>{{ t('Gelişmiş dil ayarları') }}</summary>
        <p class="muted small">{{ t('Kayıtlı tercihi olmayan ya da desteklenmeyen alıcılar ülkelerine göre bu dili alır; eşlenmemiş ülkedekiler dil kararı bekler.') }}</p>
        <p v-if="counted" class="muted small mapping-note">
          {{ t('Satırdaki sayı, o ülkede dili bu eşlemeyle belirlenen ve mesaj alabilecek alıcılardır; desteklenen dil tercihi olanlar sayılmaz.') }}
        </p>
        <div v-for="(m, i) in form.countries" :key="i" class="mapping-row">
          <select v-model="m.country" :data-test="'mapping-country-' + i" :aria-label="t('Ülke')" :disabled="busy">
            <option v-for="c in countryOptions(i)" :key="c" :value="c">{{ countryName(c) }}</option>
          </select>
          <span aria-hidden="true">→</span>
          <select v-model="m.language" :data-test="'mapping-language-' + i" :aria-label="t('Dil')" :disabled="busy">
            <option v-for="x in form.texts" :key="x.language" :value="x.language">{{ languageName(x.language) }}</option>
          </select>
          <span v-if="counted" class="mapping-count">{{ t('{n} alıcı', { n: formatCount(decided(m.country)) }) }}</span>
          <button type="button" class="mapping-remove" :data-test="'mapping-remove-' + i" :disabled="busy" @click="form.countries.splice(i, 1)">
            {{ t('Kaldır') }}
          </button>
        </div>
        <button type="button" data-test="mapping-add" :disabled="busy || !freeCountries.length" @click="addCountry">{{ t('Ülke ekle') }}</button>
      </details>
    </fieldset>

    <details class="editor-preview"><summary>{{ t('Mesaj önizlemesi') }}</summary><DraftPreview :sms="smsOf(current)" :email="emailOf(current)" /></details>
    <div v-if="error" class="error" role="alert">
      {{ error }}
      <ul v-if="issues.length" class="issues"><li v-for="(issue, i) in issues" :key="i"><code>{{ issue.path }}</code> {{ issue.message }}</li></ul>
    </div>
    <div class="actions">
      <button type="button" :disabled="busy" @click="cancel">{{ dirty ? t('Değişiklikleri geri al') : t('Kapat') }}</button>
      <button type="submit" class="primary" data-test="save-draft" :disabled="!canSave || busy">{{ busy ? t('Kaydediliyor…') : t(saveLabel) }}</button>
    </div>
  </form>
</template>
