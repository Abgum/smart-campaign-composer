<script setup lang="ts">
import { t } from '../i18n'
import { computed } from 'vue'
import type { Message } from '../api/types'
import { assistantBoldSegments, withoutScenario } from '../lib/format'

// A partial turn saved a draft before it failed: the draft stays, so there
// is nothing to retry. A failed turn saved nothing and may be retried.
const props = defineProps<{ message: Message; retryable?: boolean }>()
defineEmits<{ retry: [] }>()

// In mock mode a message may carry "#scenario:<name>" for the demo model; it
// shows as a tag, apart from the words of the request.
const scenario = computed(() => /#scenario:(\S+)/.exec(props.message.text)?.[1] ?? '')
const text = computed(() => (scenario.value ? withoutScenario(props.message.text) : props.message.text))
const assistantSegments = computed(() => assistantBoldSegments(text.value))

const badge = computed(() => {
  const m = props.message
  if (m.role !== 'assistant' || !m.llm_provider) return ''
  return m.llm_provider === 'mock' ? `MOCK · ${m.llm_model}` : `Claude · ${m.llm_model}`
})
</script>

<template>
  <div class="bubble" :class="[message.role, message.outcome]">
    <div class="text">
      <template v-if="message.role === 'assistant'">
        <template v-for="(segment, index) in assistantSegments" :key="index">
          <strong v-if="segment.bold">{{ segment.text }}</strong><span v-else>{{ segment.text }}</span>
        </template>
      </template>
      <template v-else>{{ text }}</template>
    </div>
    <span v-if="scenario" class="scenario-tag">{{ t('Demo senaryosu: {name}', { name: scenario }) }}</span>
    <div v-if="message.outcome === 'partial'" class="warning">
      {{ t('Tur yarım kaldı: kaydedilen taslak korunuyor, ama yanıt tamamlanamadı. Taslağı sağdaki panelden kontrol edin.') }}
    </div>
    <div v-else-if="message.outcome === 'failed'" class="error-note">
      {{ t('Bu istek tamamlanamadı; hiçbir şey kaydedilmedi.') }}
      <button v-if="retryable" type="button" class="link" @click="$emit('retry')">{{ t('Tekrar dene') }}</button>
    </div>
    <div v-if="badge" class="badge" :class="{ mock: message.llm_provider === 'mock' }">{{ badge }}</div>
  </div>
</template>
