<script setup lang="ts">
import { t } from '../i18n'
import { ref } from 'vue'
import type { LLMInfo } from '../api/types'
import Icon from './Icon.vue'

// The band cannot be closed: a demo must never pass the mock off as Claude.
// It stays slim; the reason opens on request.
defineProps<{ llm: LLMInfo | null }>()
const open = ref(false)
</script>

<template>
  <div v-if="llm?.provider === 'mock'" class="mock-banner" role="status">
    <Icon name="warning" :size="16" />
    <strong>{{ t("MOCK LLM: Claude'a bağlı değil") }}</strong>
    <button type="button" class="link" :aria-expanded="open" aria-controls="mock-reason" @click="open = !open">{{ t('Neden?') }}</button>
    <span v-if="open" id="mock-reason" class="mock-reason">
      {{ (llm.reason ? llm.reason + '. ' : '') + "Yanıtları demo modeli üretiyor; taslaklar yine Go'nun doğrulamasından geçer." }}
    </span>
  </div>
</template>
