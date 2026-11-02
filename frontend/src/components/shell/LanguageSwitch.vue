<script setup lang="ts">
import { locale, locales, setLocale, t } from '../../i18n'

// TR | EN: the interface language, on every screen.
</script>

<template>
  <div class="segmented lang-switch" role="group" :aria-label="t('Dil')">
    <button
      v-for="l in locales"
      :key="l.value"
      type="button"
      :lang="l.value"
      :title="l.name"
      :aria-pressed="locale === l.value"
      @click="setLocale(l.value)"
    >
      {{ l.label }}
    </button>
  </div>
</template>
