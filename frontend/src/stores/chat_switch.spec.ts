import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useChatStore } from './chat'
import type { TurnEvent } from '../api/turns'
import type { TurnResult } from '../api/types'

// The turn client reports every event with the chat its turn was sent in.
let emit: (event: TurnEvent, chatId: string) => void
const sent: string[] = []
vi.mock('../api/turns', () => ({
  TurnClient: class {
    constructor(onEvent: (event: TurnEvent, chatId: string) => void) {
      emit = onEvent
    }
    async send(chatId: string) {
      sent.push(chatId)
    }
    async resume() {
      return false
    }
  },
}))

const chats: Record<string, { messages: { id: number; turn_id: string; role: string; text: string }[] }> = {
  'chat-a': { messages: [{ id: 1, turn_id: 'old-a', role: 'user', text: 'A sohbeti' }] },
  'chat-b': { messages: [{ id: 2, turn_id: 'old-b', role: 'user', text: 'B sohbeti' }] },
}
const loaded: string[] = []
let slowA: Promise<void> | null = null // chat-a's answer waits for it
vi.mock('../api/http', () => ({
  api: {
    chat: async (id: string) => {
      if (id === 'chat-a' && slowA) await slowA
      loaded.push(id)
      return { id, messages: chats[id].messages, campaigns: [], running_turn_id: null }
    },
    turn: async () => ({ route: null }),
    campaign: async (id: string) => ({ id }),
  },
}))

const resultOfA = {
  turn_id: 'turn-a', status: 'completed', outcome: 'ok', error_code: null, route: null,
  assistant_message: { text: 'A kaydedildi', outcome: 'ok', llm_provider: 'mock', llm_model: 'demo' },
  campaign_refs: [{ campaign_id: 'campaign-of-a', version_id: 7, action: 'created' }],
} as unknown as TurnResult

beforeEach(() => {
  setActivePinia(createPinia())
  sent.length = 0
  loaded.length = 0
  slowA = null
})

describe('chat switch while a turn runs', () => {
  it('keeps the old chat turn out of the chat opened meanwhile', async () => {
    const store = useChatStore()
    await store.openChat('chat-a')
    await store.send('A için taslak hazırla')
    emit({ type: 'started', turnId: 'turn-a' }, 'chat-a')
    emit({ type: 'live', event: 'assistant.delta', data: { text: 'A yazıyor' } }, 'chat-a')

    await store.openChat('chat-b')
    loaded.length = 0
    emit({ type: 'live', event: 'assistant.delta', data: { text: ' devam' } }, 'chat-a')
    emit({ type: 'live', event: 'draft.start', data: { tool_use_id: 'tool-a' } }, 'chat-a')
    expect(store.turn).toBeNull()

    emit({ type: 'result', result: resultOfA }, 'chat-a')
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(loaded).toEqual([])
    expect(store.turn).toBeNull()
    expect(store.focusId).toBeNull()
    expect(store.messages.map((m) => m.text)).toEqual(['B sohbeti'])

    await store.send('B için soru')
    expect(sent).toEqual(['chat-a', 'chat-b'])
  })

  it('does not let a late answer for the chat left behind overwrite the open one', async () => {
    const store = useChatStore()
    let answerA!: () => void
    slowA = new Promise((resolve) => (answerA = resolve))
    const openingA = store.openChat('chat-a')
    await store.openChat('chat-b')
    answerA()
    await openingA
    expect(store.chatId).toBe('chat-b')
    expect(store.messages.map((m) => m.text)).toEqual(['B sohbeti'])
  })
})
