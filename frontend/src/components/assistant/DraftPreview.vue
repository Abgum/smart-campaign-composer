<script setup lang="ts">
import { t, tk } from '../../i18n'
import { computed } from 'vue'
import type { Email, SMS } from '../../api/types'
import { templateLength } from '../../lib/format'

// The draft as recipients will read it: the SMS on a phone, the e-mail as a
// message. A personalization shows as a named chip ("Ad"): its value differs
// per recipient and is filled in once, at approval.
const props = defineProps<{ sms: SMS | null; email: Email | null }>()

const variable = /\{\{\s*([^{}]*?)\s*\}\}/g
const names: Record<string, string> = { first_name: tk('Ad'), last_name: tk('Soyad'), city: tk('Şehir') }
type Part = { text: string; raw?: undefined } | { text: string; raw: string }

function parts(text: string): Part[] {
  const out: Part[] = []
  let at = 0
  for (const m of text.matchAll(variable)) {
    const start = m.index ?? 0
    if (start > at) out.push({ text: text.slice(at, start) })
    out.push({ text: names[m[1]] ? t(names[m[1]]) : m[1], raw: m[0] })
    at = start + m[0].length
  }
  if (at < text.length) out.push({ text: text.slice(at) })
  return out
}

// The length the draft check assumes: a variable counts as 10 characters.
const smsLength = computed(() => (props.sms ? templateLength(props.sms.text) : 0))
</script>

<template>
  <div class="previews">
    <section class="preview sms-preview" :aria-label="t('SMS önizlemesi')">
      <header class="preview-head">
        <span class="preview-title"><i class="dot-key sms" aria-hidden="true" />SMS</span>
        <span
          v-if="sms"
          class="preview-count"
          :class="{ over: smsLength > 160 }"
          :title="t('Değişkenler 10 karakter sayılır; kesin uzunluk onayda her alıcı için ölçülür.')"
        >{{ t('~{n}/160 karakter', { n: smsLength }) }}</span>
      </header>
      <div v-if="sms" class="phone">
        <p class="sms-bubble"><template v-for="(p, i) in parts(sms.text)" :key="i"><span v-if="p.raw" class="var" :title="p.raw">{{ p.text }}</span><template v-else>{{ p.text }}</template></template></p>
      </div>
      <p v-else class="preview-empty">{{ t('Bu kampanyada SMS yok.') }}</p>
    </section>

    <section class="preview email-preview" :aria-label="t('E-posta önizlemesi')">
      <header class="preview-head">
        <span class="preview-title"><i class="dot-key email" aria-hidden="true" />{{ t('E-posta') }}</span>
      </header>
      <div v-if="email" class="mail">
        <p class="mail-subject"><span class="mail-label">{{ t('Konu') }}</span><template v-for="(p, i) in parts(email.subject)" :key="i"><span v-if="p.raw" class="var" :title="p.raw">{{ p.text }}</span><template v-else>{{ p.text }}</template></template></p>
        <p class="mail-body"><template v-for="(p, i) in parts(email.body)" :key="i"><span v-if="p.raw" class="var" :title="p.raw">{{ p.text }}</span><template v-else>{{ p.text }}</template></template></p>
      </div>
      <p v-else class="preview-empty">{{ t('Bu kampanyada e-posta yok.') }}</p>
    </section>
  </div>
</template>
