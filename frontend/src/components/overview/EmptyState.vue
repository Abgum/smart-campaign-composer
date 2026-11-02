<script setup lang="ts">
import type { RouteLocationRaw } from 'vue-router'
import { RouterLink } from 'vue-router'
import Icon, { type IconName } from '../Icon.vue'

defineProps<{ icon: IconName; title: string; text: string; action?: { label: string; to: RouteLocationRaw } }>()
</script>

<template>
  <div class="empty-state card">
    <span class="empty-icon"><Icon :name="icon" :size="22" /></span>
    <h2>{{ title }}</h2>
    <p>{{ text }}</p>
    <RouterLink v-if="action" :to="action.to" class="button primary">{{ action.label }}</RouterLink>
  </div>
</template>
