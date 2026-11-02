import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { routes } from '../../router'
import AssistantHeader from './AssistantHeader.vue'
import PaneSwitch from './PaneSwitch.vue'
import DraftPreview from './DraftPreview.vue'
import ApproveBar from './ApproveBar.vue'
import ChatPanel from '../ChatPanel.vue'
import CampaignPanel from '../CampaignPanel.vue'
import WorkspaceView from '../../views/WorkspaceView.vue'
import { useChatStore } from '../../stores/chat'
import type { Meta } from '../../api/types'

const campaignId = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`

async function router(path = '/assistant/c1') {
  const r = createRouter({ history: createMemoryHistory(), routes })
  await r.push(path)
  await r.isReady()
  return r
}

describe('AssistantHeader', () => {
  it('names the chat and starts a new one', async () => {
    const w = mount(AssistantHeader, { props: { title: 'İstanbul sepet hatırlatma' }, global: { plugins: [await router()] } })
    expect(w.get('h1').text()).toBe('Asistan')
    expect(w.text()).toContain('İstanbul sepet hatırlatma')
    const link = w.findAll('a').find((a) => a.text().includes('Yeni sohbet'))
    expect(link?.attributes('href')).toBe('/assistant?new=1')
  })
})

describe('PaneSwitch', () => {
  it('switches between the chat and the draft, and counts the drafts', async () => {
    const w = mount(PaneSwitch, { props: { modelValue: 'chat', drafts: 2 } })
    const [chat, draft] = w.findAll('button')
    expect(chat.attributes('aria-pressed')).toBe('true')
    expect(draft.attributes('aria-pressed')).toBe('false')
    expect(draft.text()).toContain('Taslak')
    expect(draft.text()).toContain('2')
    await draft.trigger('click')
    expect(w.emitted('update:modelValue')?.[0]).toEqual(['draft'])
  })
})

describe('ChatPanel', () => {
  // A message could write a new version of the draft being edited, and the
  // edits would be dropped without a word; so it waits for save or undo.
  it('holds back a message while the draft has unsaved edits', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.chatId = 'c1'
    store.unsavedEdit = true
    const w = mount(ChatPanel, { global: { plugins: [pinia] } })
    await w.get('textarea').setValue("SMS'i kısalt")
    expect(w.get('button.send').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('kaydedilmemiş değişiklik')
    store.unsavedEdit = false
    await flushPromises()
    expect(w.get('button.send').attributes('disabled')).toBeUndefined()
    expect(w.text()).not.toContain('kaydedilmemiş değişiklik')
  })
})

describe('CampaignPanel', () => {
  it('keeps an unsaved editor open when another campaign tab is clicked', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    const first = campaignId(9)
    const second = campaignId(10)
    store.chatId = 'c1'
    store.focusId = first
    store.campaigns = [first, second].map((id, i) => ({ id, ordinal: i + 1, name: `Taslak ${i + 1}`, status: 'draft', version: 1, updated_at: '2026-10-28T10:00:00Z' }))
    store.details[first] = {
      id: first, chat_id: 'c1', ordinal: 1, name: 'Taslak 1', status: 'draft',
      current_version: { id: 1, version: 1, source: 'user', llm_provider: null, name: 'Taslak 1',
        segment_rule: { match: 'all', conditions: [{ kind: 'attribute', field: 'age', operator: 'gte', values: ['18'] }] },
        sms: { text: 'Merhaba' }, email: null, change_summary: '', created_at: '2026-10-28T10:00:00Z' },
      approved_version_id: null, approval: null, versions: [], updated_at: '2026-10-28T10:00:00Z',
    }
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify({ fields: [{ name: 'age', type: 'int', operators: ['gte'], description: 'Age' }], events: [], languages: [], places: [], max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20 }), { status: 200 }))
    const w = mount(CampaignPanel, { global: { plugins: [pinia] } })
    await w.get('button.edit-toggle').trigger('click')
    await flushPromises()
    await w.get('[data-test="campaign-name"]').setValue('Henüz kaydedilmedi')
    await w.get('nav.tabs').findAll('button.tab')[1]!.trigger('click')
    expect(store.focusId).toBe(first)
    expect((w.get('[data-test="campaign-name"]').element as HTMLInputElement).value).toBe('Henüz kaydedilmedi')
    expect(w.text()).toContain('Sekme değiştirmek için önce değişiklikleri kaydedin veya geri alın')
    w.unmount()
    vi.unstubAllGlobals()
  })

  it('guards an unsaved in-chat manual creation from targeting another campaign or sending', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.chatId = 'c1'
    store.focusId = campaignId(9)
    store.campaigns = [{ id: campaignId(9), ordinal: 1, name: 'Mevcut', status: 'draft', version: 1, updated_at: '2026-10-28T10:00:00Z' }]
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify({ fields: [{ name: 'age', type: 'int', operators: ['gte'], description: 'Age' }], events: [], languages: [], places: [], max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20 }), { status: 200 }))
    const w = mount(CampaignPanel, { global: { plugins: [pinia] } })
    expect(w.get('[data-test="create-manual-in-chat"]').text()).toContain('Manuel kampanya oluştur')
    await w.get('[data-test="create-manual-in-chat"]').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('Bu kampanyayı asistan kullanmadan oluşturabilirsiniz.')
    await w.get('[data-test="campaign-name"]').setValue('Yeni elle taslak')
    expect(store.unsavedEdit).toBe(true)
    await store.send('AI ile değiştir')
    expect(store.turn).toBeNull()
    await w.get('nav.tabs button.tab').trigger('click')
    expect(store.focusId).toBe(campaignId(9))
    expect((w.get('[data-test="campaign-name"]').element as HTMLInputElement).value).toBe('Yeni elle taslak')
    w.unmount()
    vi.unstubAllGlobals()
  })
  it('closes a committed edit even when the follow-up refresh fails', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    const id = campaignId(9)
    store.chatId = 'c1'
    store.focusId = id
    store.campaigns = [{ id, ordinal: 1, name: 'Taslak', status: 'draft', version: 1, updated_at: '2026-10-28T10:00:00Z' }]
    store.details[id] = {
      id, chat_id: 'c1', ordinal: 1, name: 'Taslak', status: 'draft',
      current_version: { id: 1, version: 1, source: 'user', llm_provider: null, name: 'Taslak',
        segment_rule: { match: 'all', conditions: [{ kind: 'attribute', field: 'age', operator: 'gte', values: ['18'] }] },
        sms: { text: 'Merhaba' }, email: null, change_summary: '', created_at: '2026-10-28T10:00:00Z' },
      approved_version_id: null, approval: null, versions: [], updated_at: '2026-10-28T10:00:00Z',
    }
    let edits = 0
    vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/campaign-rules/catalog') return new Response(JSON.stringify({ fields: [{ name: 'age', type: 'int', operators: ['gte'], description: 'Age' }], events: [], languages: [], places: [], max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20 }), { status: 200 })
      if (url === `/api/campaigns/${id}/versions`) { edits++; return new Response(JSON.stringify({ campaign_id: id, version_id: 2, version: 2 }), { status: 200 }) }
      return new Response(JSON.stringify({ error: 'offline', message: 'Yenileme başarısız' }), { status: 503 })
    })
    const w = mount(CampaignPanel, { global: { plugins: [pinia] } })
    await w.get('button.edit-toggle').trigger('click')
    await flushPromises()
    await w.get('[data-test="campaign-name"]').setValue('Yeni ad')
    await w.get('form.campaign-editor').trigger('submit')
    await flushPromises()
    expect(edits).toBe(1)
    expect(w.find('form.campaign-editor').exists()).toBe(false)
    expect(store.notice?.text).toContain('kaydedildi')
    w.unmount()
    vi.unstubAllGlobals()
  })
  it('restores approval after cancelling an unsaved manual edit', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    const id = campaignId(9)
    store.chatId = 'c1'
    store.focusId = id
    store.campaigns = [{ id, ordinal: 1, name: 'Taslak', status: 'draft', version: 1, updated_at: '2026-10-28T10:00:00Z' }]
    store.details[id] = {
      id, chat_id: 'c1', ordinal: 1, name: 'Taslak', status: 'draft',
      current_version: { id: 1, version: 1, source: 'llm', llm_provider: 'mock', name: 'Taslak',
        segment_rule: { match: 'all', conditions: [{ kind: 'attribute', field: 'age', operator: 'gte', values: ['18'] }] },
        sms: { text: 'Merhaba' }, email: null, audience: { total: 5, sms_reachable: 5, email_reachable: 0 },
        change_summary: '', created_at: '2026-10-28T10:00:00Z' },
      approved_version_id: null, approval: null, versions: [], updated_at: '2026-10-28T10:00:00Z',
    }
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify({ fields: [{ name: 'age', type: 'int', operators: ['gte'], description: 'Age' }], events: [], languages: [], places: [], max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20 }), { status: 200 }))
    const w = mount(CampaignPanel, { global: { plugins: [pinia] } })
    await w.get('button.edit-toggle').trigger('click')
    await flushPromises()
    await w.get('[data-test="campaign-name"]').setValue('Taslak yeni')
    expect(w.get('button.approve').attributes('disabled')).toBeDefined()
    expect(store.unsavedEdit).toBe(true)
    await w.get('.campaign-editor .actions button[type="button"]').trigger('click')
    expect(w.get('button.approve').attributes('disabled')).toBeUndefined()
    expect(store.unsavedEdit).toBe(false)
    w.unmount()
    vi.unstubAllGlobals()
  })
  it('offers manual campaign creation inside the existing chat', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.chatId = 'c1'
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify({ fields: [{ name: 'age', type: 'int', operators: ['gte'], description: 'Age' }], events: [], languages: [], places: [], max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20 }), { status: 200 }))
    const w = mount(CampaignPanel, { global: { plugins: [pinia] } })
    await w.get('[data-test="create-manual-in-chat"]').trigger('click')
    await flushPromises()
    expect(w.find('[data-test="campaign-name"]').exists()).toBe(true)
    w.unmount()
    vi.unstubAllGlobals()
  })
  it('keeps the campaign tabs and switches to the chosen campaign', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.campaigns = ['İstanbul sepet', 'Ankara hoş geldin', 'İzmir sadık', 'Antalya yaz'].map((name, i) => ({
      id: campaignId(i + 1),
      ordinal: i + 1,
      name,
      status: 'draft' as const,
      version: 1,
      updated_at: '2026-10-27T10:00:00Z',
    }))
    store.focusId = campaignId(1)
    const selectCampaign = vi.spyOn(store, 'selectCampaign').mockResolvedValue()
    const w = mount(CampaignPanel, { global: { plugins: [pinia] } })

    const tabs = w.get('nav.tabs[aria-label="Kampanyalar"]').findAll('button.tab')
    expect(tabs).toHaveLength(4)
    expect(tabs[0]?.attributes('aria-pressed')).toBe('true')
    expect(tabs[3]?.text()).toContain('Antalya yaz')
    await tabs[3]!.trigger('click')
    expect(selectCampaign).toHaveBeenCalledWith(campaignId(4))
    w.unmount()
  })

  it('keeps delivery progress in the panel without a redundant footer action', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.campaigns = [{ id: campaignId(7), ordinal: 1, name: 'İstanbul sepet', status: 'completed', version: 1, updated_at: '2026-10-27T10:00:00Z' }]
    store.focusId = campaignId(7)
    store.details[campaignId(7)] = {
      id: campaignId(7),
      chat_id: 'c1',
      ordinal: 1,
      name: 'İstanbul sepet',
      status: 'completed',
      current_version: {
        id: 10,
        version: 1,
        source: 'llm',
        llm_provider: 'mock',
        name: 'İstanbul sepet',
        segment_rule: { match: 'all', conditions: [] },
        sms: { text: 'Merhaba' },
        email: null,
        audience: { total: 1, sms_reachable: 1, email_reachable: 0 },
        change_summary: '',
        created_at: '2026-10-27T10:00:00Z',
      },
      approved_version_id: 10,
      approval: null,
      versions: [],
      updated_at: '2026-10-27T10:00:00Z',
    }
    vi.stubGlobal('fetch', async () => new Response(''))
    const w = mount(CampaignPanel, { global: { plugins: [pinia] } })
    try {
      expect(w.get('.progress h3').text()).toBe('Gönderim')
      expect(w.find('button.view-progress').exists()).toBe(false)
    } finally {
      w.unmount()
      vi.unstubAllGlobals()
    }
  })

  it('asks in its own dialog before cancelling a scheduled send', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    const id = campaignId(8)
    const future = new Date(Date.now() + 86_400_000).toISOString()
    store.campaigns = [{ id, ordinal: 2, name: 'Fransa sepet', status: 'approved', version: 1, updated_at: '2026-10-27T10:00:00Z' }]
    store.focusId = id
    store.details[id] = {
      id, chat_id: 'c1', ordinal: 2, name: 'Fransa sepet', status: 'approved', scheduled_at: future,
      current_version: { id: 11, version: 1, source: 'llm', llm_provider: 'mock', name: 'Fransa sepet',
        segment_rule: { match: 'all', conditions: [] }, sms: { text: 'Bonjour' }, email: null,
        audience: { total: 1, sms_reachable: 1, email_reachable: 0 }, change_summary: '', created_at: '2026-10-27T10:00:00Z' },
      approved_version_id: 11, approval: null, versions: [], updated_at: '2026-10-27T10:00:00Z',
    }
    const nativeConfirm = vi.fn(() => true)
    vi.stubGlobal('confirm', nativeConfirm)
    vi.stubGlobal('fetch', async () => new Response(''))
    const cancel = vi.spyOn(store, 'cancelScheduled').mockResolvedValue({ status: 'cancelled' })
    const w = mount(CampaignPanel, { global: { plugins: [pinia] }, attachTo: document.body })
    try {
      await w.get('[data-test="cancel-scheduled"]').trigger('click')
      expect(nativeConfirm).not.toHaveBeenCalled()
      const dialog = w.get('[role="alertdialog"]')
      expect(dialog.text()).toContain('Kampanya 2 · Fransa sepet')
      expect(dialog.text()).toContain('Bu kampanyanın zamanlanmış gönderimini iptal etmek istiyor musunuz?')
      await dialog.get('button:not(.confirm)').trigger('click')
      expect(w.find('[role="alertdialog"]').exists()).toBe(false)
      expect(cancel).not.toHaveBeenCalled()

      await w.get('[data-test="cancel-scheduled"]').trigger('click')
      await w.get('[role="alertdialog"] button.confirm').trigger('click')
      await flushPromises()
      expect(cancel).toHaveBeenCalledWith(id)
      expect(w.find('[role="alertdialog"]').exists()).toBe(false)
    } finally {
      w.unmount()
      vi.unstubAllGlobals()
    }
  })
})

describe('WorkspaceView', () => {
  it('selects a campaign from its public UUID query parameter', async () => {
    const id = campaignId(7)
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.chatId = 'c1'
    store.campaigns = [{ id, ordinal: 1, name: 'İstanbul sepet', status: 'draft', version: 1, updated_at: '2026-10-27T10:00:00Z' }]
    store.details[id] = {
      id, chat_id: 'c1', ordinal: 1, name: 'İstanbul sepet', status: 'draft', current_version: null,
      approved_version_id: null, approval: null, versions: [], updated_at: '2026-10-27T10:00:00Z',
    }
    const w = mount(WorkspaceView, { global: { plugins: [await router(`/assistant/c1?campaign=${id}`), pinia] } })
    await flushPromises()
    expect(store.focusId).toBe(id)
    expect(w.get('nav.tabs button.tab').attributes('aria-pressed')).toBe('true')
    w.unmount()
  })
})

describe('DraftPreview', () => {
  const sms = { text: 'Merhaba {{first_name}}, sepetin seni bekliyor!' }
  const email = { subject: '{{first_name}}, sepetin seni bekliyor', body: 'Merhaba {{first_name}},\n\nKargo bizden.' }

  it('shows the SMS and the e-mail as they will read, naming the variables', () => {
    const w = mount(DraftPreview, { props: { sms, email } })
    const bubble = w.get('.sms-preview .sms-bubble')
    expect(bubble.text()).toBe('Merhaba Ad, sepetin seni bekliyor!')
    expect(bubble.get('.var').attributes('title')).toBe('{{first_name}}')
    expect(w.get('.sms-preview').text()).toMatch(/\d+\/160 karakter/)
    expect(w.get('.email-preview .mail-subject').text()).toContain('Ad, sepetin seni bekliyor')
    expect(w.get('.email-preview .mail-body').text()).toContain('Kargo bizden.')
  })

  it('says when a channel is not in the campaign', () => {
    const w = mount(DraftPreview, { props: { sms: null, email } })
    expect(w.get('.sms-preview').text()).toContain('SMS yok')
    expect(w.find('.sms-preview .sms-bubble').exists()).toBe(false)
  })
})

describe('ApproveBar', () => {
  const audience = { total: 370, sms_reachable: 300, email_reachable: 290 }

  it('puts approval one prominent click away for a saved draft', async () => {
    const w = mount(ApproveBar, { props: { status: 'draft', dirty: false, busy: false, audience } })
    const button = w.get('button.approve')
    expect(button.classes()).toContain('primary')
    expect(button.text()).toBe('Onayla ve gönder')
    expect(button.attributes('disabled')).toBeUndefined()
    expect(w.text()).toContain('370 kişi')
    await button.trigger('click')
    expect(w.emitted('approve')).toHaveLength(1)
  })

  it('says who waits for a language decision instead of calling the draft ready', async () => {
    const w = mount(ApproveBar, { props: { status: 'draft', dirty: false, busy: false, audience, waiting: 4 } })
    expect(w.text()).not.toContain('Onaya hazır')
    expect(w.text()).toContain('4 alıcı dil kararı bekliyor')
    const button = w.get('button.approve')
    expect(button.text()).toBe('Dil kararı ver')
    expect(button.attributes('disabled')).toBeUndefined()
    await button.trigger('click')
    expect(w.emitted('approve')).toHaveLength(1)
  })

  it('asks for edits to be saved first', () => {
    const w = mount(ApproveBar, { props: { status: 'draft', dirty: true, busy: false, audience } })
    expect(w.get('button.approve').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('Önce değişiklikleri kaydedin')
  })

  it('shows where the campaign stands once approved', () => {
    const w = mount(ApproveBar, { props: { status: 'sending', dirty: false, busy: false, audience } })
    expect(w.find('button.approve').exists()).toBe(false)
    expect(w.text()).toContain('Gönderiliyor')
  })

  it('offers cancellation only before a scheduled send starts', async () => {
    const future = new Date(Date.now() + 86_400_000).toISOString()
    const w = mount(ApproveBar, { props: { status: 'approved', scheduledAt: future, dirty: false, busy: false, audience } })
    await w.get('[data-test="cancel-scheduled"]').trigger('click')
    expect(w.emitted('cancel')).toHaveLength(1)
    await w.setProps({ status: 'sending' })
    expect(w.find('[data-test="cancel-scheduled"]').exists()).toBe(false)
  })

  it('shows the final status without an unhelpful progress button or direction', () => {
    const w = mount(ApproveBar, { props: { status: 'completed', dirty: false, busy: false, audience } })
    expect(w.text()).not.toContain('aşağıda')
    expect(w.find('button.view-progress').exists()).toBe(false)
    expect(w.text()).toContain('Tamamlandı')
  })
})

describe('ChatPanel', () => {
  it('says plainly, in mock mode, that requests are not read', () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.meta = { llm: { provider: 'mock', model: 'mock-demo' } } as Meta
    const w = mount(ChatPanel, { global: { plugins: [pinia] } })
    expect(w.get('.panel-hint').text()).toContain('mesajlarınızı yorumlamaz')
  })


  it('offers example briefs in a new chat, which fill the message box', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const w = mount(ChatPanel, { global: { plugins: [pinia] } })
    const example = w.get('button.example-chip')
    await example.trigger('click')
    expect((w.get('textarea').element as HTMLTextAreaElement).value).toBe(example.text())
  })
})
