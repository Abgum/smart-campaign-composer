<script setup lang="ts">
import { t } from '../i18n'
// The selected tab is a hint for the next message: a campaign named in the
// message itself wins over it.
defineProps<{ focus: { id: string; ordinal: number; name: string } | null }>()
defineEmits<{ clear: [] }>()
</script>

<template>
  <div class="target" :class="{ empty: !focus }">
    <template v-if="focus">
      <span class="label">{{ t('Hedef:') }}</span>
      <span class="chip selected">{{ t('Kampanya {n}', { n: focus.ordinal }) }} · {{ focus.name }}</span>
      <button type="button" class="icon" :aria-label="t('Seçili kampanyayı kaldır')" :title="t('Seçimi kaldır')" @click="$emit('clear')">
        ×
      </button>
      <span class="hint">{{ t('Mesajda başka bir kampanya anarsanız o geçerli olur.') }}</span>
    </template>
    <template v-else>
      <span class="label">{{ t('Seçili kampanya yok:') }}</span>
      <span class="hint">{{ t('hedef mesajınızdan belirlenir; belirsizse size sorulur.') }}</span>
    </template>
  </div>
</template>
