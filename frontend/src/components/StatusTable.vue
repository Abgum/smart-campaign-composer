<script setup lang="ts">
import { t, tk } from '../i18n'
import { computed } from 'vue'
import type { Counts } from '../api/types'
import { channelName, orderedChannels } from '../lib/format'

// compact hides the statuses nobody is in (the sent count always shows).
const props = defineProps<{ channels: Record<string, Counts>; compact?: boolean }>()

// Ambiguous (unknown) deliveries and approval exclusions are shown apart:
// an unknown one may have reached the person and is never sent again.
const rows: { key: keyof Counts; label: string; hint?: string; tone?: string }[] = [
  { key: 'pending', label: tk('Bekliyor') },
  { key: 'in_flight', label: tk('Gönderiliyor') },
  { key: 'retry', label: tk('Tekrar denenecek') },
  { key: 'sent', label: tk('Gönderildi'), tone: 'ok' },
  { key: 'failed', label: tk('Başarısız'), hint: tk('kesin gönderilmedi'), tone: 'bad' },
  { key: 'unknown', label: tk('Belirsiz'), hint: tk('Sağlayıcı kabul etmiş olabilir; tekrar gönderilmez'), tone: 'warn' },
  { key: 'skipped_no_consent', label: tk('Atlandı: izin geri alındı') },
  { key: 'excluded_consent', label: tk('Elendi: izin yok') },
  { key: 'excluded_duplicate', label: tk('Elendi: içerik tekrarı'), hint: tk('bu içeriği zaten aldı / alacak') },
]

const names = computed(() => orderedChannels(Object.keys(props.channels)))
const empty = (key: keyof Counts) => names.value.every((c) => !props.channels[c][key])
const shown = computed(() => rows.filter((r) => !props.compact || r.key === 'sent' || !empty(r.key)))
</script>

<template>
  <table class="status-table">
    <thead>
      <tr>
        <th scope="col">{{ t('Durum') }}</th>
        <th v-for="c in names" :key="c" scope="col">{{ channelName(c) }}</th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="r in shown" :key="r.key" :class="[r.tone, { zero: empty(r.key) }]">
        <th scope="row">
          {{ t(r.label) }}<small v-if="r.hint">{{ t(r.hint) }}</small>
        </th>
        <td v-for="c in names" :key="c">{{ channels[c][r.key] }}</td>
      </tr>
    </tbody>
  </table>
</template>
