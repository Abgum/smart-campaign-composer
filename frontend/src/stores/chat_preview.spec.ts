import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useChatStore } from './chat'
import type { TurnEvent } from '../api/turns'

// Events are reported for the chat of the last turn sent, as the client does.
let emit: (event: TurnEvent) => void
vi.mock('../api/turns', () => ({
  TurnClient: class {
    chat = ''
    constructor(onEvent: (event: TurnEvent, chatId: string) => void) {
      emit = (event) => onEvent(event, this.chat)
    }
    async send(chatId: string) {
      this.chat = chatId
    }
  },
}))

beforeEach(() => setActivePinia(createPinia()))

describe('chat draft preview', () => {
  it('replaces safe preview snapshots as the tool streams', async () => {
    const store = useChatStore()
    store.chatId = 'chat-1'
    await store.send("SMS'i kısalt")
    emit({ type: 'live', event: 'draft.start', data: { tool_use_id: 'tool-1' } })
    emit({ type: 'live', event: 'draft.delta', data: { tool_use_id: 'tool-1', partial_json: '{"name":"Se"}', replace: true } })
    emit({ type: 'live', event: 'draft.delta', data: { tool_use_id: 'tool-1', partial_json: '{"name":"Sepet","sms":{"text":"Mer"}}', replace: true } })

    expect(store.turn?.preview.drafts[0]?.partial).toBe('{"name":"Sepet","sms":{"text":"Mer"}}')
  })

  it('discards only the failed model iteration and retains earlier preview', async () => {
    const store = useChatStore()
    store.chatId = 'chat-1'
    await store.send('Bir kampanya hazırla')
    emit({ type: 'live', event: 'iteration.started', data: { iteration: 1 } })
    emit({ type: 'live', event: 'assistant.delta', data: { text: 'Önceki geçerli metin. ' } })
    emit({ type: 'live', event: 'draft.start', data: { tool_use_id: 'saved' } })
    emit({ type: 'live', event: 'draft.saved', data: { tool_use_id: 'saved', campaign_id: 'c1' } })
    emit({ type: 'live', event: 'iteration.started', data: { iteration: 2 } })
    emit({ type: 'live', event: 'assistant.delta', data: { text: 'Yarım metin' } })
    emit({ type: 'live', event: 'draft.start', data: { tool_use_id: 'partial' } })
    emit({ type: 'live', event: 'draft.delta', data: { tool_use_id: 'partial', partial_json: '{"sms":', replace: true } })
    emit({ type: 'live', event: 'iteration.discarded', data: { iteration: 2 } })
    expect(store.turn?.preview.text).toBe('Önceki geçerli metin. ')
    expect(store.turn?.preview.drafts.map((d) => d.toolUseId)).toEqual(['saved'])
  })
})
