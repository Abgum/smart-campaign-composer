<script setup lang="ts">
import { t } from '../i18n'
import type { TargetRef } from '../api/types'

defineProps<{ candidates: TargetRef[] }>()
defineEmits<{ choose: [id: string] }>()
</script>

<template>
  <div class="chips" role="group" :aria-label="t('Hangi kampanya?')">
    <button v-for="c in candidates" :key="c.campaign_id" type="button" class="chip" @click="$emit('choose', c.campaign_id)">
      {{ t('Kampanya {n}', { n: c.ordinal }) }} · {{ c.name }}
    </button>
  </div>
</template>
