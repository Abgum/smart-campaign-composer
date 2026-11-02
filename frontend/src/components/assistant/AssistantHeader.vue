<script setup lang="ts">
import { t } from '../../i18n'
import { RouterLink } from 'vue-router'
import Icon from '../Icon.vue'

// The assistant's title row: which chat this is, and a way to start afresh.
defineProps<{ title: string }>()
</script>

<template>
  <header class="assistant-head">
    <div class="assistant-title">
      <h1 class="page-title">{{ t('Asistan') }}</h1>
      <p class="assistant-sub" :title="title">{{ title }}</p>
    </div>
    <RouterLink :to="{ name: 'workspace', query: { new: '1' } }" class="button new-chat">
      <Icon name="plus" :size="16" />{{ t('Yeni sohbet') }}
    </RouterLink>
  </header>
</template>
