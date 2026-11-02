import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { routes } from '../../router'
import { useLiveStore } from '../../stores/live'
import CampaignList from './CampaignList.vue'
import FilterCard from './FilterCard.vue'
import RecentRequests from './RecentRequests.vue'
import OverviewView from '../../views/OverviewView.vue'
import type { Analytics, Counts, Dashboard, RecentRequest, TrackedCampaign } from '../../api/types'

const campaignId = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`

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

const sending: TrackedCampaign = {
  id: campaignId(7),
  chat_id: 'c1',
  ordinal: 1,
  name: 'İstanbul sepet',
  status: 'sending',
  updated_at: '2026-10-27T10:00:00Z',
  approved_at: '2026-10-27T09:00:00Z',
  last_sent_at: '2026-10-27T10:00:00Z',
  version: 3,
  llm_provider: 'mock',
  channels: {
    sms: counts({ sent: 120, failed: 2, unknown: 1, pending: 52, excluded_consent: 79 }),
    email: counts({ sent: 130, pending: 65, excluded_duplicate: 3 }),
  },
}
const done: TrackedCampaign = {
  ...sending,
  id: campaignId(8),
  chat_id: 'c2',
  ordinal: 2,
  name: 'Ankara hoş geldin',
  status: 'completed',
  channels: { email: counts({ sent: 40 }) },
}
const draft: TrackedCampaign = { ...sending, id: campaignId(9), ordinal: 3, name: 'Antalya yaz', status: 'draft', approved_at: null, channels: {} }

async function router(path = '/') {
  const r = createRouter({ history: createMemoryHistory(), routes })
  await r.push(path)
  await r.isReady()
  return r
}

const request = (r: Partial<RecentRequest>): RecentRequest => ({
  turn_id: 't1',
  chat_id: 'c1',
  text: 'İstanbul için sepet hatırlatması',
  status: 'completed',
  outcome: 'ok',
  intent: 'create',
  campaigns: [],
  llm_provider: 'anthropic',
  created_at: '2026-10-27T10:00:00Z',
  ...r,
})

describe('RecentRequests', () => {
  it("shows what marketers asked and what came of it, linked to the chat", async () => {
    const w = mount(RecentRequests, {
      props: {
        requests: [
          request({ text: 'Ankara için hoş geldin #scenario:new_campaign', campaigns: [{ campaign_id: campaignId(2), name: 'Ankara hoş geldin', ordinal: 2, version: 1, action: 'created' }] }),
          request({ turn_id: 't2', text: 'SMS’i kısalt', intent: 'edit', campaigns: [{ campaign_id: campaignId(1), name: 'İstanbul sepet', ordinal: 1, version: 3, action: 'updated' }] }),
          request({ turn_id: 't3', text: 'Kampanyayı değiştir', intent: 'clarify' }),
          request({ turn_id: 't4', text: 'Yeni bir fikir', status: 'running', outcome: '', intent: '' }),
          request({ turn_id: 't5', text: 'Olmadı', status: 'failed', outcome: 'failed' }),
        ],
      },
      global: { plugins: [await router()] },
    })
    const items = w.findAll('.request-item')
    expect(items[0].get('.request-text').text()).toBe('“Ankara için hoş geldin”') // the mock's scenario tag is not the request
    expect(items.map((i) => i.get('.result-chip').text())).toEqual([
      'Ankara hoş geldin · oluşturuldu',
      'İstanbul sepet · güncellendi (v3)',
      'Soru soruldu',
      'Hazırlanıyor',
      'Tamamlanamadı',
    ])
    expect(items[0].get('a').attributes('href')).toBe('/assistant/c1')
  })

  it('does not pass a mock answer off as a reading of the brief', async () => {
    const w = mount(RecentRequests, {
      props: {
        requests: [
          request({
            text: 'Antalya için yaz kampanyası taslağı',
            llm_provider: 'mock',
            campaigns: [{ campaign_id: campaignId(4), name: 'İstanbul sepet hatırlatma', ordinal: 4, version: 1, action: 'created' }],
          }),
          request({ turn_id: 't2', text: 'Kampanyayı değiştir', intent: 'clarify', llm_provider: 'mock' }),
        ],
      },
      global: { plugins: [await router()] },
    })
    const [created, asked] = w.findAll('.result-chip')
    expect(created.classes()).toContain('tone-mock')
    expect(created.text()).toBe('Mock · hazır demo taslağı: İstanbul sepet hatırlatma')
    expect(created.attributes('title')).toContain('isteği yorumlamaz')
    expect(asked.text()).toBe('Mock · Soru soruldu')
  })

  it('invites a first request', async () => {
    const w = mount(RecentRequests, { props: { requests: [] }, global: { plugins: [await router()] } })
    expect(w.text()).toContain('Henüz istek yok')
  })
})

describe('CampaignList', () => {
  it('shows each campaign with its progress, or that a draft awaits approval', async () => {
    const w = mount(CampaignList, { props: { campaigns: [sending, draft] }, global: { plugins: [await router()] } })
    const items = w.findAll('.campaign-item')
    expect(items[0].get('.campaign-item-name').text()).toBe('İstanbul sepet')
    expect(items[0].get('.status-chip').text()).toBe('Gönderiliyor')
    expect(items[0].get('.progress-text').text()).toBe('250/370 gönderildi · 2 başarısız · 1 belirsiz')
    expect(items[0].find('.segment-bar').exists()).toBe(true)
    expect(items[1].text()).toContain('Onay bekliyor')
    expect(items[0].get('a.campaign-item-name').attributes('href')).toBe(`/assistant/c1?campaign=${campaignId(7)}`)
  })

  it('narrows to the outcome a KPI picked, and opens who was left out', async () => {
    const w = mount(CampaignList, { props: { campaigns: [sending, done, draft], status: 'failed' }, global: { plugins: [await router()] } })
    const items = w.findAll('.campaign-item')
    expect(items.map((i) => i.get('.campaign-item-name').text())).toEqual(['İstanbul sepet'])
    await items[0].get('button.expand').trigger('click')
    const detail = w.get('.breakdown').text()
    expect(detail).toContain('79 izin yok')
    expect(detail).toContain('3 içerik tekrarı')
  })
})

describe('FilterCard', () => {
  it('emits the selected public campaign UUID', async () => {
    const id = campaignId(7)
    const w = mount(FilterCard, {
      props: { filters: { range: '7d', channel: '', campaign: null }, campaigns: [{ id, label: 'İstanbul sepet' }] },
    })
    await w.get('select[name="campaign"]').setValue(id)
    expect(w.emitted('update')?.at(-1)?.[0]).toEqual({ range: '7d', channel: '', campaign: id })
  })
})

const analytics = (a: Partial<Analytics['kpis']> = {}, sentDays: [string, number, number][] = []): Analytics => ({
  window: { range: '7d', from: '2026-10-21', to: '2026-10-27', tz: 'Europe/Istanbul', bucket: 'day' },
  kpis: {
    sent: { total: 0, sms: 0, email: 0 },
    queued: { total: 0, pending: 0, in_flight: 0, retry: 0, sms: 0, email: 0 },
    failed: { total: 0, sms: 0, email: 0 },
    unknown: { total: 0, sms: 0, email: 0 },
    ...a,
  },
  series: ['21', '22', '23', '24', '25', '26', '27'].map((d) => {
    const hit = sentDays.find(([day]) => day === d)
    return { at: `2026-10-${d}`, sms: hit?.[1] ?? 0, email: hit?.[2] ?? 0 }
  }),
  cities: { top: [], other: { cities: 0, total: 0 }, unknown: { total: 0, sms: 0, email: 0 } },
})

const board = (alive: boolean): Dashboard => ({
  at: '2026-10-27T12:00:00Z',
  campaigns: [sending],
  totals: {},
  channels: { sms: { due: 52, calls: 100, window_ms: 1050, paused_until: null } },
  workers: [{ name: 'w1', beat_at: '2026-10-27T11:59:59Z', alive }],
  version: 'v1',
})

describe('OverviewView', () => {
  let calls: string[]
  function serve(a: Analytics, campaigns: TrackedCampaign[], requests: RecentRequest[] = []) {
    calls = []
    vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
      const url = String(input)
      calls.push(url)
      const body = url.startsWith('/api/analytics') ? a : url.startsWith('/api/requests') ? { requests } : { campaigns }
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    })
  }
  beforeEach(() => setActivePinia(createPinia()))
  afterEach(() => vi.unstubAllGlobals())

  async function view(path: string, live: Dashboard | null) {
    const r = await router(path)
    const pinia = createPinia()
    setActivePinia(pinia)
    if (live) useLiveStore().board = live
    const w = mount(OverviewView, { global: { plugins: [r, pinia] } })
    await flushPromises()
    return { w, r }
  }

  it('asks the API for the slice in the URL and shows its KPIs', async () => {
    serve(analytics({ sent: { total: 1240, sms: 1000, email: 240 } }, [['27', 1000, 240]]), [sending])
    const { w } = await view('/?range=30d&channel=sms', board(true))
    expect(calls).toContain('/api/analytics?range=30d&cities=10&channel=sms')
    expect(w.findAll('.stat-tile').map((t) => t.get('.tile-label').text())).toEqual(['Gönderildi', 'Kuyrukta', 'Başarısız', 'Belirsiz'])
    expect(w.get('.stat-tile .value').text()).toBe('1.240')
  })

  it('tells scheduled messages apart from the queue', async () => {
    serve(analytics({ queued: { total: 4, pending: 4, in_flight: 0, retry: 0, sms: 4, email: 0, scheduled: 370 } }, [['27', 1, 0]]), [sending])
    const { w } = await view('/', board(true))
    const queued = w.findAll('.stat-tile').find((t) => t.get('.tile-label').text() === 'Kuyrukta')
    expect(queued?.get('.value').text()).toBe('4')
    expect(queued?.text()).toContain('370 mesaj zamanlanmış')
  })

  it('puts a changed filter in the URL and refetches', async () => {
    serve(analytics({ sent: { total: 5, sms: 5, email: 0 } }, [['27', 5, 0]]), [sending])
    const { w, r } = await view('/', board(true))
    const before = calls.length
    await w.get('button[data-range="today"]').trigger('click')
    await flushPromises()
    expect(r.currentRoute.value.query.range).toBe('today')
    expect(calls.slice(before).some((c) => c.includes('range=today'))).toBe(true)
  })

  it('opens the one day with sends by hour when it is not today', async () => {
    serve(analytics({ sent: { total: 4, sms: 3, email: 1 } }, [['25', 3, 1]]), [sending])
    const { w, r } = await view('/', board(true))
    const note = w.get('.chart-note')
    expect(note.text()).toContain('25 Ekim')
    expect(note.get('button').text()).toBe('O günü saatlik gör')
    const before = calls.length
    await note.get('button').trigger('click')
    await flushPromises()
    expect(r.currentRoute.value.query).toEqual({ range: 'day', day: '2026-10-25' })
    expect(calls.slice(before)).toContain('/api/analytics?range=day&day=2026-10-25&cities=10')
  })

  it('opens today by hour when the one day with sends is today', async () => {
    serve(analytics({ sent: { total: 4, sms: 3, email: 1 } }, [['27', 3, 1]]), [sending])
    const { w, r } = await view('/', board(true))
    const button = w.get('.chart-note button')
    expect(button.text()).toBe('Bugünü saatlik gör')
    await button.trigger('click')
    await flushPromises()
    expect(r.currentRoute.value.query).toEqual({ range: 'today' })
  })

  it('names the day it shows by hour, in the title and the filter card', async () => {
    const hours = Array.from({ length: 24 }, (_, h) => ({ at: `2026-10-25T${String(h).padStart(2, '0')}:00`, sms: h === 14 ? 3 : 0, email: 0 }))
    serve(
      {
        ...analytics({ sent: { total: 3, sms: 3, email: 0 } }),
        window: { range: 'day', from: '2026-10-25', to: '2026-10-25', tz: 'Europe/Istanbul', bucket: 'hour' },
        series: hours,
      },
      [sending],
    )
    const { w } = await view('/?range=day&day=2026-10-25', board(true))
    expect(calls).toContain('/api/analytics?range=day&day=2026-10-25&cities=10')
    expect(w.get('.chart-card .card-title').text()).toBe('25 Ekim saatlik gönderim')
    const day = w.get('button[data-range="day"]')
    expect(day.text()).toBe('25 Eki')
    expect(day.attributes('aria-pressed')).toBe('true')
    expect(w.findAll('button[data-range][aria-pressed="true"]')).toHaveLength(1)
    expect(w.find('.chart-note').exists()).toBe(false)
  })

  it('lists the latest requests and the campaigns at the bottom', async () => {
    serve(analytics({ sent: { total: 5, sms: 5, email: 0 } }, [['27', 5, 0]]), [sending, draft], [request({})])
    const { w } = await view('/', board(true))
    expect(calls).toContain('/api/requests?limit=6')
    expect(w.findAll('.request-item')).toHaveLength(1)
    expect(w.findAll('.campaign-item').map((i) => i.get('.campaign-item-name').text())).toEqual(['İstanbul sepet', 'Antalya yaz'])
  })

  it('invites a first campaign when nothing was ever approved', async () => {
    serve(analytics(), [])
    const { w } = await view('/', null)
    const empty = w.get('.empty-state')
    expect(empty.text()).toContain('Henüz gönderim yok')
    // "Yeni kampanya" goes where it goes from the menu and the campaign list:
    // the page that offers both the editor and the assistant.
    expect(empty.get('a').attributes('href')).toBe('/campaigns/new')
    expect(w.get('a.hero-new').attributes('href')).toBe('/campaigns/new')
    expect(w.find('.columns-chart').exists()).toBe(false)
  })

  it('says when the range has no sends, with shortcuts', async () => {
    serve(analytics(), [done])
    const { w } = await view('/', board(true))
    expect(w.text()).toContain('Bu aralıkta gönderim yok')
    expect(w.findAll('.range-shortcuts button').map((b) => b.text())).toContain('Tümü')
  })

  it('says plainly when sending has stopped', async () => {
    serve(analytics({ queued: { total: 117, pending: 117, in_flight: 0, retry: 0, sms: 52, email: 65 } }, [['27', 1, 0]]), [sending])
    const { w } = await view('/', board(false))
    expect(w.get('.alert-strip').text()).toContain('Gönderim şu an ilerlemiyor')
    expect(w.text()).not.toContain('worker')
  })
})
