<script setup lang="ts">
import { t } from '../../i18n'
import type { Counts } from '../../api/types'
import StatusTable from '../StatusTable.vue'

// An A/B campaign's outcomes, arm by arm: each recipient got one arm, the
// same on both channels, so the two tables add up to the campaign's own.
defineProps<{ variants: Record<string, Record<string, Counts>> }>()
const arms = ['A', 'B']
</script>

<template>
  <section class="card ab-results">
    <header class="card-head">
      <h2 class="card-title">{{ t('A/B sonuçları') }}</h2>
      <p class="card-sub">{{ t('Her alıcı tek varyant aldı, iki kanalda da aynısını. Sayılar bu kampanyanın teslimatlarıdır.') }}</p>
    </header>
    <div v-for="arm in arms" :key="arm" class="ab-arm">
      <h3>{{ t('Varyant {arm}', { arm }) }}</h3>
      <StatusTable :channels="variants[arm] ?? {}" compact />
    </div>
  </section>
</template>
