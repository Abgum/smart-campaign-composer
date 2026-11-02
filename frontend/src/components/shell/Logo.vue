<script setup lang="ts">
import { useId } from 'vue'

// The mark: a speech bubble (the brief and the messages it becomes) with a
// spark in it (the model drafting), on the accent's soft purple.
withDefaults(defineProps<{ size?: number }>(), { size: 34 })
const gradient = `${useId()}-logo`
</script>

<template>
  <svg class="logo" :width="size" :height="size" viewBox="0 0 32 32" aria-hidden="true" focusable="false">
    <defs>
      <linearGradient :id="gradient" x1="0" y1="0" x2="32" y2="32" gradientUnits="userSpaceOnUse">
        <stop offset="0" stop-color="#8b7df6" />
        <stop offset="1" stop-color="#5443d4" />
      </linearGradient>
    </defs>
    <rect width="32" height="32" rx="9" :fill="`url(#${gradient})`" />
    <path
      d="M12 8h8a4 4 0 0 1 4 4v4a4 4 0 0 1-4 4h-6.4l-3.75 3.1a.6.6 0 0 1-.98-.46V19.5A4 4 0 0 1 8 16v-4a4 4 0 0 1 4-4z"
      fill="#fff"
    />
    <path
      d="M16 10.4c.36 2.05 1.5 3.2 3.55 3.6-2.05.4-3.19 1.55-3.55 3.6-.36-2.05-1.5-3.2-3.55-3.6 2.05-.4 3.19-1.55 3.55-3.6z"
      fill="#6c5ce7"
    />
  </svg>
</template>
