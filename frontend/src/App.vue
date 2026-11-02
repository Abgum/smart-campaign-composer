<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { RouterView, useRoute } from 'vue-router'
import { api } from './api/http'
import type { ChatSummary } from './api/types'
import { useChatStore } from './stores/chat'
import { useLiveStore } from './stores/live'
import AppShell from './components/shell/AppShell.vue'

const chat = useChatStore()
const live = useLiveStore()
const route = useRoute()
const chats = ref<ChatSummary[]>([])

async function loadChats() {
  try {
    chats.value = await api.chats(12)
  } catch {
    // the list is a shortcut; the app works without it
  }
}

onMounted(async () => {
  live.start()
  await chat.loadMeta()
  await loadChats()
})
// A new chat, or a finished turn that named a campaign, changes the list.
watch(() => [route.params.chatId, chat.campaigns.length], loadChats)
</script>

<template>
  <AppShell :llm="chat.meta?.llm ?? null" :workers="live.workers" :connected="live.connected" :chats="chats">
    <RouterView />
  </AppShell>
</template>
