import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import MockBanner from './MockBanner.vue'
import TargetIndicator from './TargetIndicator.vue'
import AmbiguousChips from './AmbiguousChips.vue'
import MessageBubble from './MessageBubble.vue'
import TurnPreview from './TurnPreview.vue'
import ApproveModal from './ApproveModal.vue'
import StatusTable from './StatusTable.vue'
import { ApiError } from '../api/http'
import type { CampaignDetail, Counts, Message } from '../api/types'
import { localInputValue } from '../lib/schedule'

const campaignId = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`

describe('MockBanner', () => {
  it('says the mock answers, gives the reason on request, and cannot be closed', async () => {
    const w = mount(MockBanner, { props: { llm: { provider: 'mock', model: 'mock-demo', reason: 'ANTHROPIC_API_KEY tanımlı değil' } } })
    expect(w.text()).toContain("MOCK LLM: Claude'a bağlı değil")
    const why = w.get('button')
    expect(why.text()).toBe('Neden?')
    expect(why.attributes('aria-expanded')).toBe('false')
    await why.trigger('click')
    expect(why.attributes('aria-expanded')).toBe('true')
    expect(w.text()).toContain('ANTHROPIC_API_KEY tanımlı değil. Yanıtları demo modeli üretiyor')
    expect(w.findAll('button')).toHaveLength(1) // nothing closes it
  })

  it('is absent with Claude', () => {
    const w = mount(MockBanner, { props: { llm: { provider: 'anthropic', model: 'claude-opus-5-5' } } })
    expect(w.text()).toBe('')
  })
})

describe('TargetIndicator', () => {
  it('shows the selected campaign and clears it', async () => {
    const w = mount(TargetIndicator, { props: { focus: { id: campaignId(7), ordinal: 2, name: 'İzmir Yaz' } } })
    expect(w.text()).toContain('Kampanya 2')
    expect(w.text()).toContain('İzmir Yaz')
    await w.get('button').trigger('click')
    expect(w.emitted('clear')).toHaveLength(1)
  })

  it('says when no campaign is selected', () => {
    const w = mount(TargetIndicator, { props: { focus: null } })
    expect(w.text()).toContain('Seçili kampanya yok')
    expect(w.find('button').exists()).toBe(false)
  })
})

describe('AmbiguousChips', () => {
  it('offers each candidate and reports the choice', async () => {
    const w = mount(AmbiguousChips, {
      props: {
        candidates: [
          { campaign_id: campaignId(11), ordinal: 1, name: 'İstanbul Sepet' },
          { campaign_id: campaignId(12), ordinal: 2, name: 'İzmir Yaz' },
        ],
      },
    })
    const chips = w.findAll('button')
    expect(chips.map((c) => c.text())).toEqual(['Kampanya 1 · İstanbul Sepet', 'Kampanya 2 · İzmir Yaz'])
    await chips[1].trigger('click')
    expect(w.emitted('choose')).toEqual([[campaignId(12)]])
  })
})

const message = (m: Partial<Message>): Message => ({
  turn_id: 't1',
  role: 'assistant',
  text: 'Yanıt',
  outcome: 'ok',
  llm_provider: 'mock',
  llm_model: 'mock-demo',
  created_at: '2026-10-27T10:00:00Z',
  ...m,
})

describe('MessageBubble', () => {
  it('formats assistant bold labels while treating model HTML as text', () => {
    const w = mount(MessageBubble, {
      props: { message: message({ text: '- **Hedef kitle:** İstanbul\n- **SMS:** Merhaba {{first_name}} <img src=x onerror=alert(1)>' }) },
    })
    expect(w.findAll('.text strong').map((node) => node.text())).toEqual(['Hedef kitle:', 'SMS:'])
    expect(w.get('.text').text()).not.toContain('**')
    expect(w.find('.text img').exists()).toBe(false)
    expect(w.get('.text').text()).toContain('<img src=x onerror=alert(1)>')
  })

  it('keeps user Markdown markers literal', () => {
    const w = mount(MessageBubble, { props: { message: message({ role: 'user', text: 'Bunu **kalın** yaz', outcome: null }) } })
    expect(w.get('.text').text()).toBe('Bunu **kalın** yaz')
    expect(w.find('.text strong').exists()).toBe(false)
  })

  it('names the model that answered', () => {
    expect(mount(MessageBubble, { props: { message: message({}) } }).text()).toContain('mock-demo')
    const claude = mount(MessageBubble, { props: { message: message({ llm_provider: 'anthropic', llm_model: 'claude-opus-5-5' }) } })
    expect(claude.text()).toContain('claude-opus-5-5')
  })

  it('warns on a partial turn and offers no retry: the saved draft stays', () => {
    const w = mount(MessageBubble, {
      props: { message: message({ outcome: 'partial', text: "Kampanya 2 v3 kaydedildi. Claude'un yanıtı tamamlanamadı." }), retryable: true },
    })
    expect(w.find('.warning').exists()).toBe(true)
    expect(w.text()).toContain('Kampanya 2 v3 kaydedildi')
    expect(w.find('button').exists()).toBe(false)
  })

  it('shows the mock scenario a message asked for as a tag, not as words of the brief', () => {
    const w = mount(MessageBubble, {
      props: { message: message({ role: 'user', text: 'Antalya için yaz kampanyası #scenario:new_campaign', outcome: null, llm_provider: null, llm_model: null }) },
    })
    expect(w.get('.text').text()).toBe('Antalya için yaz kampanyası')
    expect(w.get('.scenario-tag').text()).toBe('Demo senaryosu: new_campaign')
  })

  it('offers a retry for a failed turn', async () => {
    const w = mount(MessageBubble, { props: { message: message({ outcome: 'failed', text: 'Yanıt üretilemedi.' }), retryable: true } })
    await w.get('button').trigger('click')
    expect(w.emitted('retry')).toHaveLength(1)
  })
})

describe('TurnPreview', () => {
  it('formats bold labels in the streamed assistant text', () => {
    const w = mount(TurnPreview, {
      props: {
        turn: {
          phase: 'streaming', userText: '', turnId: 't1',
          preview: { route: null, text: '- **SMS:** Merhaba', drafts: [], error: null, notice: null },
        },
      },
    })
    expect(w.get('.assistant .text strong').text()).toBe('SMS:')
    expect(w.get('.assistant .text').text()).not.toContain('**')
  })

  it('streams the email body with the subject, as the draft is written', () => {
    const partial = '{"name":"Sepet","email":{"subject":"Sepetin seni bekliyor","body":"Merhaba Ayşe,\\nürünlerin hâlâ sepet"}}'
    const w = mount(TurnPreview, {
      props: {
        turn: {
          phase: 'streaming', userText: '', turnId: 't1',
          preview: { route: null, text: '', drafts: [{ toolUseId: 'tool-1', partial }], error: null, notice: null },
        },
      },
    })
    const rows = w.findAll('.glimpse-row').map((r) => r.text())
    expect(rows).toContain('E-posta konusu: Sepetin seni bekliyor')
    expect(rows).toContain('E-posta metni: Merhaba Ayşe,\nürünlerin hâlâ sepet')
  })
})

const campaign: CampaignDetail = {
  id: campaignId(5),
  chat_id: 'c1',
  ordinal: 2,
  name: 'İstanbul sepet (kopya)',
  status: 'draft',
  current_version: {
    id: 42,
    version: 1,
    source: 'llm',
    llm_provider: 'mock',
    name: 'İstanbul sepet (kopya)',
    segment_rule: { match: 'all', conditions: [] },
    sms: { text: 'Merhaba {{first_name}}' },
    email: null,
    audience: { total: 3, sms_reachable: 3, email_reachable: 0 },
    change_summary: 'duplicate',
    created_at: '2026-10-27T10:00:00Z',
  },
  approved_version_id: null,
  approval: null,
  versions: [],
  updated_at: '2026-10-27T10:00:00Z',
}

describe('ApproveModal', () => {
  it('shows which model wrote the draft, then who is left out and why', async () => {
    const approve = vi.fn().mockResolvedValue({
      campaign_id: campaignId(5),
      version_id: 42,
      status: 'approved',
      already_approved: false,
      channels: { sms: { selected: 1, excluded_consent: 1, excluded_duplicate: 1 } },
    })
    const w = mount(ApproveModal, { props: { campaign, approve } })
    expect(w.text()).toContain('MOCK LLM')
    await w.get('button.confirm').trigger('click')
    await flushPromises()

    expect(approve).toHaveBeenCalledWith(42)
    expect(w.text()).toContain('1 kişi bu içeriği zaten aldı / alacak; gönderilmeyecek')
    expect(w.text()).toContain('1 kişi izin ya da iletişim bilgisi olmadığı için elendi')
    expect(w.emitted('approved')).toHaveLength(1)
  })

  it('sends now by default and schedules a later moment in UTC on request', async () => {
    const later = new Date(Date.now() + 2 * 24 * 60 * 60 * 1000)
    later.setHours(14, 30, 0, 0)
    const local = localInputValue(later)
    const iso = later.toISOString()
    const approve = vi.fn().mockResolvedValue({
      campaign_id: campaignId(5),
      version_id: 42,
      status: 'approved',
      already_approved: false,
      channels: { sms: { selected: 1, excluded_consent: 0, excluded_duplicate: 0 } },
      scheduled_at: iso,
    })
    const w = mount(ApproveModal, { props: { campaign, approve } })
    expect(w.get<HTMLInputElement>('input[name="send-when"][value="now"]').element.checked).toBe(true)
    expect(w.get('button.confirm').text()).toBe('Onayla ve kuyruğa al')

    await w.get('input[name="send-when"][value="later"]').setValue()
    await w.get('input[type="datetime-local"]').setValue(local)
    expect(w.get('button.confirm').text()).toBe('Onayla ve zamanla')
    await w.get('button.confirm').trigger('click')
    await flushPromises()

    expect(approve).toHaveBeenCalledWith(42, { scheduled_at: iso })
    expect(w.text()).toContain('Onaylandı; gönderim')
    expect(w.text()).toContain(String(later.getFullYear()))
  })

  it('will not schedule more than a year ahead', async () => {
    const approve = vi.fn()
    const w = mount(ApproveModal, { props: { campaign, approve } })
    await w.get('input[name="send-when"][value="later"]').setValue()
    await w.get('input[type="datetime-local"]').setValue(localInputValue(new Date(Date.now() + 400 * 24 * 60 * 60 * 1000)))
    expect(w.get('button.confirm').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('Gönderim zamanı en fazla bir yıl sonrası olabilir.')
  })

  it('will not schedule a moment that has passed', async () => {
    const approve = vi.fn()
    const w = mount(ApproveModal, { props: { campaign, approve } })
    await w.get('input[name="send-when"][value="later"]').setValue()
    await w.get('input[type="datetime-local"]').setValue('2000-01-01T10:00')

    expect(w.get('button.confirm').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('Gönderim zamanı gelecekte olmalı.')
    await w.get('button.confirm').trigger('click')
    expect(approve).not.toHaveBeenCalled()
  })

  it('explains a stale version instead of approving', async () => {
    const approve = vi.fn().mockRejectedValue(new ApiError(409, 'stale_version', 'kampanya bu arada değişti; güncel versiyonu yükleyip tekrar deneyin'))
    const w = mount(ApproveModal, { props: { campaign, approve } })
    await w.get('button.confirm').trigger('click')
    await flushPromises()

    expect(w.text()).toContain('kampanya bu arada değişti')
    expect(w.emitted('approved')).toBeUndefined()
  })
})

describe('StatusTable', () => {
  const counts = (c: Partial<Counts>): Counts => ({
    excluded_consent: 0,
    excluded_duplicate: 0,
    pending: 0,
    in_flight: 0,
    retry: 0,
    sent: 0,
    failed: 0,
    unknown: 0,
    skipped_no_consent: 0,
    ...c,
  })

  it('shows ambiguous deliveries and approval exclusions apart', () => {
    const w = mount(StatusTable, { props: { channels: { sms: counts({ sent: 40, unknown: 2, excluded_duplicate: 3 }), email: counts({ sent: 5 }) } } })
    const row = (label: string) => w.findAll('tr').find((r) => r.text().startsWith(label))?.findAll('td').map((td) => td.text())
    expect(row('Belirsiz')).toEqual(['2', '0'])
    expect(row('Gönderildi')).toEqual(['40', '5'])
    expect(row('Elendi: içerik tekrarı')).toEqual(['3', '0'])
  })

  it('in compact form hides the statuses nobody is in, but always shows the sent count', () => {
    const w = mount(StatusTable, { props: { compact: true, channels: { sms: counts({ unknown: 2 }), email: counts({}) } } })
    const labels = w.findAll('tbody th').map((th) => th.text())
    expect(labels.some((l) => l.startsWith('Belirsiz'))).toBe(true)
    expect(labels.some((l) => l.startsWith('Gönderildi'))).toBe(true)
    expect(labels.some((l) => l.startsWith('Bekliyor'))).toBe(false)
  })
})
