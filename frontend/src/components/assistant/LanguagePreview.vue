<script setup lang="ts">
import { t } from '../../i18n'
import { computed, nextTick, ref, watch } from 'vue'
import type { ABTest, Email, LanguageSummary, Localization, SMS } from '../../api/types'
import { formatCount } from '../../lib/format'
import { countryName, groupLabel, languageName } from '../../lib/languages'
import DraftPreview from './DraftPreview.vue'

// The draft as its readers get it. One content for everyone names its
// language; content in each recipient's language has a tab per variant with
// how many recipients read it (as of the draft: those it can message, each
// once). Recipients who wait for a language decision are listed: approval
// stays locked until each group is decided. An A/B test adds an A|B switch:
// the language picks the content, the arm picks A or B of it.
const props = defineProps<{
  localization?: Localization | null
  summary?: LanguageSummary | null
  sms: SMS | null
  email: Email | null
  abTest?: ABTest | null
}>()
const arm = ref<'A' | 'B'>('A')

const variants = computed(() => (props.localization?.mode === 'per_recipient' ? (props.localization.variants ?? []) : []))
const active = ref(0)
watch(
  () => variants.value.map((v) => v.language).join(),
  () => {
    if (active.value >= variants.value.length) active.value = 0
  },
)
const shown = computed(() => variants.value[active.value] ?? null)
// B of the content shown: by its language, or the one B of one content.
const bShown = computed(() => {
  const bs = props.abTest?.variants ?? []
  return shown.value ? (bs.find((b) => b.language === shown.value?.language) ?? null) : (bs[0] ?? null)
})
const inB = computed(() => !!props.abTest && arm.value === 'B')
// Who has no supported preference gets the language of their country: the
// mapping and how many recipients each line decides (those the campaign can
// message), under advanced settings.
const mapping = computed(() => (variants.value.length ? (props.localization?.country_languages ?? []) : []))
// A version saved before the counts were kept has none: no number then.
const counted = computed(() => props.summary?.countries != null)
const decided = (country: string) => props.summary?.countries?.[country] ?? 0
const reach = (lang: string) => props.summary?.languages?.[lang]?.total
const waiting = computed(() => props.summary?.unresolved ?? [])
const waitingTotal = computed(() => waiting.value.reduce((n, g) => n + g.count, 0))

// Arrow keys move between the tabs, as in any tab list.
const tabs = ref<HTMLButtonElement[]>([])
async function onKey(e: KeyboardEvent) {
  const n = variants.value.length
  const step = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0
  if (!step || !n) return
  e.preventDefault()
  active.value = (active.value + step + n) % n
  await nextTick()
  tabs.value[active.value]?.focus()
}
</script>

<template>
  <div class="language-preview">
    <p v-if="localization?.mode === 'single' && localization.language" class="language-single">
      {{ t('Herkese aynı metin · {language}', { language: languageName(localization.language) }) }}
    </p>
    <template v-if="abTest">
      <div class="arm-tabs" role="tablist" :aria-label="t('A/B varyantları')">
        <button
          v-for="x in (['A', 'B'] as const)"
          :key="x"
          type="button"
          role="tab"
          :data-test="'preview-arm-' + x"
          :aria-selected="arm === x"
          :class="{ active: arm === x }"
          @click="arm = x"
        >{{ t('Varyant {arm}', { arm: x }) }}</button>
      </div>
      <p class="muted small ab-note">{{ t("A/B testi: alıcıların yaklaşık yarısı A'yı, yarısı B'yi alır; bir alıcı iki kanalda da aynı varyantı alır.") }}</p>
    </template>
    <template v-if="variants.length">
      <div class="variant-tabs" role="tablist" :aria-label="t('Dil varyantları')">
        <button
          v-for="(v, i) in variants"
          :key="v.language"
          ref="tabs"
          type="button"
          role="tab"
          :aria-selected="i === active"
          :tabindex="i === active ? 0 : -1"
          :class="{ active: i === active }"
          @click="active = i"
          @keydown="onKey"
        >{{ languageName(v.language) }}<template v-if="reach(v.language) !== undefined"> · {{ formatCount(reach(v.language) ?? 0) }}</template></button>
      </div>
      <p v-if="summary?.languages" class="muted small language-scope">
        {{ t('Sayılar taslak anına ait, onayda yeniden hesaplanır: kampanyanın açık kanallarından en az birinden mesaj alabilecek alıcılar; iki kanala da uygun olan bir kez sayılır.') }}
      </p>
      <div role="tabpanel" :aria-label="shown ? languageName(shown.language) : undefined">
        <DraftPreview :sms="(inB ? bShown : shown)?.sms ?? null" :email="(inB ? bShown : shown)?.email ?? null" />
      </div>
    </template>
    <DraftPreview v-else :sms="inB ? (bShown?.sms ?? null) : sms" :email="inB ? (bShown?.email ?? null) : email" />
    <div v-if="waiting.length" class="language-waiting" role="note">
      <strong>{{ t('{n} alıcı dil kararı bekliyor; karar verilene kadar onay kilitli.', { n: formatCount(waitingTotal) }) }}</strong>
      <ul>
        <li v-for="g in waiting" :key="g.group">{{ groupLabel(g.group) }}: {{ formatCount(g.count) }}</li>
      </ul>
    </div>
    <details v-if="mapping.length" class="language-advanced">
      <summary>{{ t('Gelişmiş dil ayarları') }}</summary>
      <p class="muted small">{{ t('Ülke eşlemesi: kayıtlı dil tercihi olmayan ya da desteklenmeyen alıcı, ülkesine göre bu dili alır.') }}</p>
      <ul class="mapping-list">
        <li v-for="c in mapping" :key="c.country">
          {{ countryName(c.country) }} → {{ languageName(c.language)
          }}<template v-if="counted"> · {{ t('{n} alıcı', { n: formatCount(decided(c.country)) }) }}</template>
        </li>
      </ul>
      <p v-if="counted" class="muted small mapping-note">
        {{ t('Satırdaki sayı, o ülkede dili bu eşlemeyle belirlenen ve mesaj alabilecek alıcılardır; desteklenen dil tercihi olanlar sayılmaz.') }}
      </p>
    </details>
  </div>
</template>
