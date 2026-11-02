<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api/http'
import { useChatStore } from '../stores/chat'
import { withoutScenario } from '../lib/format'
import { t } from '../i18n'
import ChatPanel from '../components/ChatPanel.vue'
import CampaignPanel from '../components/CampaignPanel.vue'
import AssistantHeader from '../components/assistant/AssistantHeader.vue'
import PaneSwitch from '../components/assistant/PaneSwitch.vue'

// The assistant: a chat and its campaigns. /assistant opens the current chat,
// else the most recent one (a new one only when there is none), /assistant?new=1
// starts a new one, and ?campaign=<id> selects a campaign of the chat.
const route = useRoute()
const router = useRouter()
const store = useChatStore()

async function sync() {
  const id = typeof route.params.chatId === 'string' ? route.params.chatId : ''
  try {
    if (!id || route.query.new) {
      let next = route.query.new ? '' : (store.chatId ?? (await api.chats(1).catch(() => []))[0]?.id ?? '')
      if (!next) next = await store.newChat()
      await router.replace({ name: 'workspace', params: { chatId: next } })
      return
    }
    if (id !== store.chatId) await store.openChat(id)
  } catch {
    const next = await store.newChat()
    await router.replace({ name: 'workspace', params: { chatId: next } })
    return
  }
  const campaign = typeof route.query.campaign === 'string' ? route.query.campaign : ''
  if (campaign && store.campaigns.some((c) => c.id === campaign)) {
    await store.selectCampaign(campaign)
  }
}

watch(() => [route.params.chatId, route.query.new, route.query.campaign], sync, { immediate: true })

// The chat is named as in the chat list: its first campaign, else what was
// asked first.
const title = computed(() => {
  const asked = store.messages.find((m) => m.role === 'user')
  return store.campaigns[0]?.name ?? (asked ? withoutScenario(asked.text) : t('Yeni sohbet'))
})

// On narrow screens one side shows at a time; a campaign opened from
// elsewhere (?campaign=) shows its draft.
const pane = ref<'chat' | 'draft'>(route.query.campaign ? 'draft' : 'chat')
watch(
  () => route.query.campaign,
  (k) => k && (pane.value = 'draft'),
)
</script>

<template>
  <section class="page assistant-page">
    <AssistantHeader :title="title" />
    <PaneSwitch v-model="pane" :drafts="store.campaigns.length" />
    <div class="workspace" :data-pane="pane">
      <ChatPanel />
      <CampaignPanel />
    </div>
  </section>
</template>
