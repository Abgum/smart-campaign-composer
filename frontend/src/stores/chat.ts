import { defineStore } from 'pinia'
import { tk } from '../i18n'
import { computed, ref } from 'vue'
import { api } from '../api/http'
import { TurnClient, type TurnEvent } from '../api/turns'
import type { ApproveOptions, Audience, CampaignDetail, CampaignSummary, Issue, ManualDraftInput, Message, Meta, RouteView, TurnResult } from '../api/types'

// The live preview of a running turn. It is never merged into the stored
// state: when the turn ends, the UI reloads what the API stored.
export interface DraftPreview {
  toolUseId: string
  partial: string
  saved?: { campaign_id: string; ordinal: number; version: number; created: boolean; audience: Audience }
  invalid?: Issue[]
}

export interface Preview {
  route: RouteView | null
  text: string
  drafts: DraftPreview[]
  error: { code: string; message: string } | null
  notice: string | null
  iteration?: { number: number; text: string; draftCount: number } | null
}

export interface TurnState {
  phase: 'sending' | 'streaming' | 'waiting'
  userText: string
  turnId: string | null
  preview: Preview
}

const emptyPreview = (): Preview => ({ route: null, text: '', drafts: [], error: null, notice: null, iteration: null })

const resetNotice: Record<string, string> = {
  lagged: tk('Bağlantı yavaş olduğu için canlı önizleme durduruldu; tur bitince kayıtlı sonuç gösterilecek.'),
  disconnected: tk('Bağlantı koptu. Tur sunucuda sürüyor; kayıtlı sonuç bekleniyor…'),
  result_pending: tk('Tur sonucu henüz kaydedilmedi; kayıtlı sonuç bekleniyor…'),
}

export const useChatStore = defineStore('chat', () => {
  const meta = ref<Meta | null>(null)
  const chatId = ref<string | null>(null)
  const messages = ref<Message[]>([])
  const campaigns = ref<CampaignSummary[]>([])
  const details = ref<Record<string, CampaignDetail>>({})
  const focusId = ref<string | null>(null)
  const turn = ref<TurnState | null>(null)
  const notice = ref<{ text: string; reconnect?: boolean } | null>(null)
  const lastRoute = ref<RouteView | null>(null) // route of the latest finished turn
  // unsavedEdit says the draft panel holds manual edits not saved yet; a
  // message could write a newer version and drop them, so it waits.
  const unsavedEdit = ref(false)

  const client = new TurnClient(onTurnEvent)

  const focus = computed(() => campaigns.value.find((c) => c.id === focusId.value) ?? null)
  const isMock = computed(() => meta.value?.llm.provider === 'mock')
  // The clarifying question of the latest turn, with its candidates.
  const question = computed(() => {
    const r = lastRoute.value
    return r && r.intent === 'clarify' && (r.candidates?.length ?? 0) > 0 && !turn.value ? r : null
  })

  function current(): TurnState {
    turn.value ??= { phase: 'waiting', userText: '', turnId: null, preview: emptyPreview() }
    return turn.value
  }

  // A turn's events count only while its chat is open. A turn left behind by
  // opening another chat goes on on the server; opening its chat again picks
  // it up (resume).
  function onTurnEvent(e: TurnEvent, chat: string) {
    if (chat !== chatId.value) return
    switch (e.type) {
      case 'started': {
        const t = current()
        t.turnId = e.turnId
        t.phase = 'streaming'
        break
      }
      case 'live':
        applyLive(current().preview, e.event, e.data)
        break
      case 'preview-reset': {
        const t = current()
        t.preview = emptyPreview()
        t.preview.notice = e.message ?? resetNotice[e.reason]
        break
      }
      case 'waiting': {
        const t = current()
        t.phase = 'waiting'
        t.turnId = e.turnId
        t.preview.notice ??= tk('İstek sunucuda sürüyor; kayıtlı sonuç bekleniyor…')
        break
      }
      case 'result':
        void finishTurn(e.result)
        break
      case 'failed':
        turn.value = null
        notice.value = { text: e.message, reconnect: e.code === 'network' }
        break
    }
  }

  function applyLive(p: Preview, event: string, data: any) {
    const draft = (id: string) => p.drafts.find((d) => d.toolUseId === id)
    switch (event) {
      case 'iteration.started':
        p.iteration = { number: data.iteration, text: p.text, draftCount: p.drafts.length }
        break
      case 'iteration.discarded':
        if (p.iteration && p.iteration.number === data.iteration) {
          const checkpoint = p.iteration
          p.text = checkpoint.text
          p.drafts.splice(checkpoint.draftCount)
          p.iteration = null
        }
        break
      case 'route':
        p.route = data as RouteView
        break
      case 'assistant.delta':
        p.text += data.text ?? ''
        break
      case 'draft.start':
        p.drafts.push({ toolUseId: data.tool_use_id, partial: '' })
        break
      case 'draft.delta': {
        const d = draft(data.tool_use_id)
        if (d) d.partial = data.replace ? (data.partial_json ?? '') : d.partial + (data.partial_json ?? '')
        break
      }
      case 'draft.saved': {
        const d = draft(data.tool_use_id)
        if (d) d.saved = data
        break
      }
      case 'draft.invalid': {
        const d = draft(data.tool_use_id)
        if (d) d.invalid = data.errors
        break
      }
      case 'error':
        p.error = data
        break
    }
  }

  // finishTurn shows what the API stored for the turn: the transcript and
  // the versions it wrote. A campaign the turn wrote becomes the selected
  // tab, so the next message continues on it (and the indicator says so).
  async function finishTurn(res: TurnResult) {
    const chat = chatId.value
    lastRoute.value = res.route ?? null
    try {
      if (chat) await loadChat(chat)
      if (chatId.value !== chat) return // another chat was opened meanwhile
      const last = res.campaign_refs.at(-1)
      if (last) focusId.value = last.campaign_id
      await Promise.all(res.campaign_refs.map((r) => loadCampaign(r.campaign_id).catch(() => {})))
    } catch {
      if (chatId.value === chat) notice.value = { text: tk('Tur bitti ama sohbet yenilenemedi; sayfayı yenileyin.') }
    } finally {
      if (chatId.value === chat) turn.value = null
    }
  }

  async function loadMeta() {
    try {
      meta.value = await api.meta()
    } catch {
      notice.value = { text: tk('API okunamadı; sunucunun çalıştığından emin olun.') }
    }
  }

  // newChat creates a chat and opens it; the caller puts its id in the URL.
  async function newChat(): Promise<string> {
    const { id } = await api.createChat()
    await openChat(id)
    return id
  }

  async function openChat(id: string) {
    chatId.value = id
    focusId.value = null
    details.value = {}
    turn.value = null
    notice.value = null
    lastRoute.value = null
    const chat = await loadChat(id)
    if (chatId.value !== id) return // another chat was opened meanwhile
    const lastAssistant = [...chat.messages].reverse().find((m) => m.role === 'assistant')
    if (lastAssistant) {
      api
        .turn(lastAssistant.turn_id)
        .then((r) => {
          if (!turn.value && chatId.value === id) lastRoute.value = r.route ?? null
        })
        .catch(() => {})
    }
    // A turn left running or pending by a reload is picked up again.
    void client.resume(id, chat.running_turn_id)
  }

  // loadChat shows a chat's messages and campaigns, unless another chat was
  // opened while they loaded.
  async function loadChat(id: string) {
    const chat = await api.chat(id)
    if (chatId.value === id) {
      messages.value = chat.messages
      campaigns.value = chat.campaigns
    }
    return chat
  }

  async function loadCampaign(id: string) {
    details.value[id] = await api.campaign(id)
  }

  async function send(text: string) {
    const t = text.trim()
    if (!chatId.value || turn.value || unsavedEdit.value || !t) return
    notice.value = null
    lastRoute.value = null
    turn.value = { phase: 'sending', userText: t, turnId: null, preview: emptyPreview() }
    await client.send(chatId.value, t, focusId.value)
  }

  // retry sends the text of a failed turn again, as a new turn.
  async function retry(turnId: string) {
    const user = messages.value.find((m) => m.turn_id === turnId && m.role === 'user')
    if (user) await send(user.text)
  }

  // choose answers a clarifying question: the chosen campaign becomes the
  // selected tab and the question's message is sent again.
  async function choose(campaignId: string) {
    const asked = [...messages.value].reverse().find((m) => m.role === 'user')
    await selectCampaign(campaignId)
    if (asked) await send(asked.text)
  }

  async function reconnect() {
    if (!chatId.value) return
    notice.value = null
    await client.resume(chatId.value, null)
  }

  async function selectCampaign(id: string | null) {
    if (unsavedEdit.value && id !== focusId.value) return
    focusId.value = id
    if (id !== null && !details.value[id]) await loadCampaign(id).catch(() => {})
  }

  async function saveEdit(id: string, baseVersionId: number, edit: ManualDraftInput) {
    const saved = await api.editCampaign(id, { base_version_id: baseVersionId, ...edit })
    const refreshed = await Promise.allSettled([loadCampaign(id), chatId.value ? loadChat(chatId.value) : Promise.resolve()])
    if (refreshed.some((result) => result.status === 'rejected')) {
      notice.value = { text: tk('Kampanya kaydedildi, ancak görünüm yenilenemedi. Sayfayı yenileyin.') }
    }
    return saved
  }

  async function createManual(draft: ManualDraftInput) {
    if (!chatId.value || turn.value) throw new Error(tk('Sohbet hazır değil'))
    const created = await api.createCampaign({ chat_id: chatId.value, ...draft })
    focusId.value = created.campaign_id
    const refreshed = await Promise.allSettled([loadChat(chatId.value), loadCampaign(created.campaign_id)])
    if (refreshed.some((result) => result.status === 'rejected')) {
      notice.value = { text: tk('Kampanya kaydedildi, ancak görünüm yenilenemedi. Sayfayı yenileyin.') }
    }
    return created
  }

  async function approve(id: string, versionId: number, options?: ApproveOptions) {
    try {
      return await api.approve(id, versionId, options)
    } finally {
      await Promise.all([loadCampaign(id).catch(() => {}), chatId.value ? loadChat(chatId.value) : null])
    }
  }

  async function cancelScheduled(id: string) {
    try {
      return await api.cancelScheduled(id)
    } finally {
      await Promise.all([loadCampaign(id).catch(() => {}), chatId.value ? loadChat(chatId.value) : null])
    }
  }

  return {
    meta,
    chatId,
    messages,
    campaigns,
    details,
    focusId,
    focus,
    turn,
    notice,
    lastRoute,
    unsavedEdit,
    question,
    isMock,
    loadMeta,
    newChat,
    openChat,
    send,
    retry,
    choose,
    reconnect,
    selectCampaign,
    loadCampaign,
    saveEdit,
    createManual,
    approve,
    cancelScheduled,
  }
})


