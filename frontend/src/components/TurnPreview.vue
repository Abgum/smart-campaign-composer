<script setup lang="ts">
import { t } from '../i18n'
import { computed } from 'vue'
import type { TurnState } from '../stores/chat'
import { glimpse } from '../lib/glimpse'
import { assistantBoldSegments } from '../lib/format'
import RouteLine from './RouteLine.vue'

// The running turn as it streams. Everything here is a preview; the stored
// result replaces it when the turn ends.
const props = defineProps<{ turn: TurnState }>()

const drafts = computed(() => props.turn.preview.drafts.map((d) => ({ ...d, glimpse: glimpse(d.partial) })))
const assistantSegments = computed(() => assistantBoldSegments(props.turn.preview.text))
</script>

<template>
  <div class="turn-preview">
    <div v-if="turn.userText" class="bubble user pending">
      <div class="text">{{ turn.userText }}</div>
    </div>
    <div class="bubble assistant live">
      <RouteLine v-if="turn.preview.route" :route="turn.preview.route" />
      <div v-if="turn.preview.notice" class="notice-line">{{ t(turn.preview.notice) }}</div>
      <div v-if="turn.preview.text" class="text">
        <template v-for="(segment, index) in assistantSegments" :key="index">
          <strong v-if="segment.bold">{{ segment.text }}</strong><span v-else>{{ segment.text }}</span>
        </template><span class="cursor" />
      </div>
      <div v-for="d in drafts" :key="d.toolUseId" class="draft-glimpse">
        <div class="draft-title">
          <span v-if="d.saved">
            ✓ {{ t('Kampanya {n} v{v} kaydedildi · kitle {total} kişi', { n: d.saved.ordinal, v: d.saved.version, total: d.saved.audience.total }) }}
          </span>
          <span v-else-if="d.invalid" class="invalid">{{ t('Taslak doğrulanamadı; asistan düzeltmeye çalışıyor') }}</span>
          <span v-else>{{ t('Taslak yazılıyor…') }}</span>
        </div>
        <div v-if="d.glimpse.name" class="glimpse-row"><b>{{ t('Ad:') }}</b> {{ d.glimpse.name }}</div>
        <div v-if="d.glimpse.sms" class="glimpse-row"><b>{{ t('SMS:') }}</b> {{ d.glimpse.sms }}</div>
        <div v-if="d.glimpse.subject" class="glimpse-row"><b>{{ t('E-posta konusu:') }}</b> {{ d.glimpse.subject }}</div>
        <div v-if="d.glimpse.body" class="glimpse-row"><b>{{ t('E-posta metni:') }}</b> {{ d.glimpse.body }}</div>
        <ul v-if="d.invalid" class="issues">
          <li v-for="(is, i) in d.invalid" :key="i">{{ is.message }}</li>
        </ul>
      </div>
      <div v-if="turn.preview.error" class="error-note">{{ t(turn.preview.error.message) }}</div>
      <div v-if="!turn.preview.text && !drafts.length && !turn.preview.notice" class="thinking">
        {{ turn.phase === 'sending' ? t('Gönderiliyor…') : t('Düşünüyor…') }}
      </div>
      <div class="live-label">{{ t('canlı önizleme · kesin sonuç tur bitince gösterilir') }}</div>
    </div>
  </div>
</template>
