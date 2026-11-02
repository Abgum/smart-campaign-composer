<script setup lang="ts">
import { t } from '../i18n'
import { computed } from 'vue'
import type { RouteView, TargetRef } from '../api/types'

// The verified routing decision of a turn: what it did and to which
// campaigns, so the user can see which campaign a message changed.
const props = defineProps<{ route: RouteView }>()

const names = (refs: TargetRef[] | undefined) => (refs ?? []).map((r) => `${t('Kampanya {n}', { n: r.ordinal })} (${r.name})`).join(', ')

const text = computed(() => {
  const r = props.route
  switch (r.intent) {
    case 'create':
      return r.references?.length ? t('Yeni kampanya · kaynak: {names}', { names: names(r.references) }) : t('Yeni kampanya')
    case 'edit':
      return t('Hedef: {names}', { names: names(r.targets) })
    case 'clarify':
      return t('Hedef belirsiz: soruluyor')
    default:
      return t('Sohbet: kampanya değişmez')
  }
})
</script>

<template>
  <div class="route-line" :class="route.intent">
    {{ text }}
    <span v-if="route.source === 'fallback'" class="muted"> {{ t('· router karar veremedi') }}</span>
  </div>
</template>
