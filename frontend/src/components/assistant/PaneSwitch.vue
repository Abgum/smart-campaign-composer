<script setup lang="ts">
import { t } from '../../i18n'
import Icon from '../Icon.vue'

// Below tablet width the chat and the draft take turns on the screen; the
// draft side shows how many campaigns the chat has.
defineProps<{ modelValue: 'chat' | 'draft'; drafts: number }>()
defineEmits<{ 'update:modelValue': [pane: 'chat' | 'draft'] }>()
</script>

<template>
  <div class="segmented pane-switch" role="group" :aria-label="t('Görünüm')">
    <button type="button" :aria-pressed="modelValue === 'chat'" @click="$emit('update:modelValue', 'chat')">
      <Icon name="assistant" :size="16" />{{ t('Sohbet') }}
    </button>
    <button type="button" :aria-pressed="modelValue === 'draft'" @click="$emit('update:modelValue', 'draft')">
      <Icon name="mail" :size="16" />{{ t('Taslak') }}<span v-if="drafts" class="count-badge">{{ drafts }}</span>
    </button>
  </div>
</template>
