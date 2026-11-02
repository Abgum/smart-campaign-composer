import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import { createMemoryHistory, createRouter } from 'vue-router'
import { routes } from '../router'
import ApproveModal from './ApproveModal.vue'
import ConfirmDialog from './ConfirmDialog.vue'
import AppShell from './shell/AppShell.vue'
import type { AnalyticsCities, ApproveResult, CampaignDetail, SeriesPoint } from '../api/types'
import { createPinia, setActivePinia } from 'pinia'
import ChatPanel from './ChatPanel.vue'
import StackedColumns from './charts/StackedColumns.vue'
import CityBars from './charts/CityBars.vue'
import EChart from './charts/EChart.vue'
import { useChatStore } from '../stores/chat'
import { useLiveStore } from '../stores/live'
import OverviewView from '../views/OverviewView.vue'
import CampaignDetailView from '../views/CampaignDetailView.vue'
import { cityOption, cityText, dailyText } from '../lib/chartOptions'
import { focusables } from '../lib/dialog'

// Keyboard and screen-reader behaviour of the dialogs and the app frame.

const mounted: VueWrapper[] = []
afterEach(() => {
  mounted.splice(0).forEach((w) => w.unmount())
  document.body.innerHTML = ''
})

const campaign: CampaignDetail = {
  id: '00000000-0000-4000-8000-000000000005',
  chat_id: 'c1',
  ordinal: 2,
  name: 'İstanbul sepet',
  status: 'draft',
  current_version: {
    id: 31,
    version: 3,
    source: 'llm',
    llm_provider: 'mock',
    name: 'İstanbul sepet',
    segment_rule: { match: 'all', conditions: [] },
    sms: { text: 'Merhaba' },
    email: null,
    audience: { total: 10, sms_reachable: 8, email_reachable: 0 },
    change_summary: '',
    created_at: '2026-10-27T08:00:00Z',
  },
  approved_version_id: null,
  approval: null,
  versions: [],
  updated_at: '2026-10-27T09:00:00Z',
}
const approved: ApproveResult = {
  campaign_id: campaign.id,
  version_id: 31,
  status: 'approved',
  already_approved: false,
  channels: { sms: { selected: 8, excluded_consent: 2, excluded_duplicate: 0 } },
}

function modal(approve = async () => approved) {
  const w = mount(ApproveModal, { props: { campaign, approve }, attachTo: document.body })
  mounted.push(w)
  return w
}
const active = () => document.activeElement as HTMLElement | null

describe('approve dialog', () => {
  it('takes focus when it opens, on the safe choice', async () => {
    const w = modal()
    await nextTick()
    expect(w.get('.modal').element.contains(active())).toBe(true)
    expect(active()?.textContent?.trim()).toBe('Vazgeç')
  })

  it('keeps Tab and Shift+Tab inside, and closes on Esc', async () => {
    const w = modal()
    await nextTick()
    // every control Tab reaches, the sending-time choice included
    const controls = focusables(w.get('.modal').element as HTMLElement)
    const first = controls[0]
    const last = controls[controls.length - 1]
    expect(first.closest('.modal')).not.toBeNull()
    last.focus()
    await w.get('.modal').trigger('keydown', { key: 'Tab' })
    expect(active()).toBe(first)
    await w.get('.modal').trigger('keydown', { key: 'Tab', shiftKey: true })
    expect(active()).toBe(last)
    await w.get('.modal').trigger('keydown', { key: 'Escape' })
    expect(w.emitted('close')).toHaveLength(1)
  })

  it('moves focus to the result once approved, and gives it back when closed', async () => {
    const opener = document.createElement('button')
    opener.textContent = 'Onayla ve gönder'
    document.body.appendChild(opener)
    opener.focus()
    const w = modal()
    await nextTick()
    await w.get('button.confirm').trigger('click')
    await flushPromises()
    expect(active()?.textContent?.trim()).toBe('Tamam')
    w.unmount()
    mounted.splice(mounted.indexOf(w), 1)
    expect(active()).toBe(opener)
  })
})

describe('confirm dialog', () => {
  function confirmDialog(props: Record<string, unknown> = {}) {
    const w = mount(ConfirmDialog, {
      props: { title: 'Kampanya 2 · İstanbul sepet', confirmLabel: 'Gönderimi iptal et', busyLabel: 'İptal ediliyor…', danger: true, ...props },
      slots: { default: '<p>Emin misiniz?</p>' },
      attachTo: document.body,
    })
    mounted.push(w)
    return w
  }

  it('is an alert dialog named by its title and described by its text', () => {
    const w = confirmDialog()
    const box = w.get('.modal')
    expect(box.attributes('role')).toBe('alertdialog')
    expect(box.attributes('aria-modal')).toBe('true')
    expect(document.getElementById(box.attributes('aria-labelledby')!)?.textContent).toBe('Kampanya 2 · İstanbul sepet')
    expect(document.getElementById(box.attributes('aria-describedby')!)?.textContent).toContain('Emin misiniz?')
  })

  it('takes focus on the safe choice, closes on Esc, the backdrop or Vazgeç', async () => {
    const w = confirmDialog()
    await nextTick()
    expect(active()?.textContent?.trim()).toBe('Vazgeç')
    await w.get('.modal').trigger('keydown', { key: 'Escape' })
    await w.get('.modal-backdrop').trigger('click')
    await w.get('.modal button:not(.confirm)').trigger('click')
    expect(w.emitted('close')).toHaveLength(3)
    expect(w.emitted('confirm')).toBeUndefined()
  })

  it('asks for the action with a danger button, and says so while it runs', async () => {
    const w = confirmDialog()
    const go = w.get('button.confirm')
    expect(go.classes()).toContain('danger')
    expect(go.text()).toBe('Gönderimi iptal et')
    await go.trigger('click')
    expect(w.emitted('confirm')).toHaveLength(1)
    await w.setProps({ busy: true })
    expect(go.text()).toBe('İptal ediliyor…')
    expect(go.attributes('disabled')).toBeDefined()
  })
})

async function shellAt(path: string) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  const w = mount(AppShell, {
    global: { plugins: [router] },
    props: { llm: { provider: 'mock', model: 'mock-demo' }, workers: { alive: 1, total: 1 }, connected: true, chats: [] },
    slots: { default: '<h1>Sayfa</h1>' },
    attachTo: document.body,
  })
  mounted.push(w)
  return { w, router }
}

describe('app frame', () => {
  it('starts with a link that skips the menu to the content', async () => {
    const { w } = await shellAt('/')
    const skip = w.findAll('a')[0]
    expect(skip.text()).toBe('İçeriğe geç')
    await skip.trigger('click')
    expect(active()).toBe(w.get('main').element)
  })

  it('opens the menu drawer as a modal: focus in, the page behind out of reach, Esc back to the menu button', async () => {
    const { w } = await shellAt('/')
    await w.get('.menu-button').trigger('click')
    await nextTick()
    expect(active()).toBe(w.get('.drawer-close').element)
    expect(w.get('.main-column').attributes('inert')).toBeDefined()
    await w.get('.sidebar').trigger('keydown', { key: 'Escape' })
    await nextTick()
    expect(w.get('.shell').classes()).not.toContain('drawer-open')
    expect(w.get('.main-column').attributes('inert')).toBeUndefined()
    expect(active()).toBe(w.get('.menu-button').element)
  })

  it('leaves focus at the top when the app first opens a page', async () => {
    const router = createRouter({ history: createMemoryHistory(), routes })
    const w = mount(AppShell, {
      global: { plugins: [router] },
      props: { llm: null, workers: null, connected: true, chats: [] },
      attachTo: document.body,
    })
    mounted.push(w)
    await router.push('/campaigns') // the first navigation, as on a page load
    await flushPromises()
    expect(active()).not.toBe(w.get('main').element)
  })

  it('moves focus to the content when the page changes, not when only the query does', async () => {
    const { w, router } = await shellAt('/')
    await router.push('/campaigns')
    await flushPromises()
    expect(active()).toBe(w.get('main').element)
    ;(w.get('.menu-button').element as HTMLElement).focus()
    await router.push('/campaigns?status=draft')
    await flushPromises()
    expect(active()).toBe(w.get('.menu-button').element)
  })
})

describe('chat for screen readers', () => {
  it('is a log that reads out new messages once, not the stream as it types, and scrolls by keyboard', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useChatStore()
    store.turn = {
      phase: 'streaming',
      userText: 'İstanbul için SMS',
      turnId: 't1',
      preview: { route: null, text: 'Taslak hazırlan', drafts: [], error: null, notice: null },
    }
    const w = mount(ChatPanel, { global: { plugins: [pinia] } })
    const log = w.get('.messages')
    expect(log.attributes('role')).toBe('log')
    expect(log.attributes('aria-relevant')).toBe('additions')
    expect(log.attributes('aria-label')).toBe('Sohbet mesajları')
    expect(log.attributes('tabindex')).toBe('0')
    expect(w.find('.turn-preview').exists()).toBe(true)
    expect(log.findAll('[aria-live]')).toHaveLength(0)
  })
})

const day = (at: string, sms: number, email: number): SeriesPoint => ({ at, sms, email })
const points = [day('2026-10-25', 50, 25), day('2026-10-26', 0, 0), day('2026-10-27', 100, 0)]
const cities: AnalyticsCities = {
  top: [{ geoname_id: 745044, name: 'İstanbul', country: 'TR', admin1: 'TR-34', sms: 3, email: 1, total: 4 }],
  other: { cities: 0, total: 0 },
  unknown: { total: 2, sms: 2, email: 0 },
}

describe('charts by keyboard', () => {
  it('reads a column or a row out as plain text', () => {
    expect(dailyText(points, 'day', ['sms', 'email'], 0)).toMatch(/^25 Eki.*: toplam 75; SMS 50, E-posta 25$/)
    const { rows } = cityOption(cities, ['sms', 'email'])
    expect(cityText(rows[0], 6, ['sms', 'email'])).toMatch(/^İstanbul: 4 gönderim \(.*\); SMS 3, E-posta 1$/)
    expect(cityText(rows[1], 6, ['sms', 'email'])).toContain('şehir kaydı yok')
  })

  it('hands the chart one stop per column, and its words', () => {
    const w = mount(StackedColumns, { props: { points, bucket: 'day' }, global: { stubs: { EChart: true } } })
    const chart = w.getComponent(EChart)
    expect(chart.props('items')).toBe(3)
    expect(chart.props('describe')?.(2)).toBe(dailyText(points, 'day', ['sms', 'email'], 2))
    const bars = mount(CityBars, { props: { cities }, global: { stubs: { EChart: true } } }).getComponent(EChart)
    expect(bars.props('items')).toBe(2)
    expect(bars.props('describe')?.(0)).toContain('İstanbul')
  })
})

function serveJSON(routes: [string, unknown][]) {
  vi.stubGlobal('fetch', async (input: RequestInfo | URL) => {
    const url = String(input)
    const hit = routes.find(([prefix]) => url.startsWith(prefix))
    return new Response(JSON.stringify(hit ? hit[1] : {}), { status: 200, headers: { 'Content-Type': 'application/json' } })
  })
}

describe('pages', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('names a campaign page after the campaign', async () => {
    serveJSON([
      [`/api/campaigns/${campaign.id}`, campaign],
      ['/api/campaigns?', { campaigns: [] }],
    ])
    const router = createRouter({ history: createMemoryHistory(), routes })
    await router.push(`/campaigns/${campaign.id}`)
    await router.isReady()
    const pinia = createPinia()
    setActivePinia(pinia)
    mounted.push(mount(CampaignDetailView, { global: { plugins: [router, pinia] } }))
    await flushPromises()
    expect(document.title).toBe('İstanbul sepet · Kampanyalar · Smart Campaign Composer')
  })

  it('jumps instead of gliding to the list when the system asks for less motion', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: q.includes('reduce'), addEventListener() {}, removeEventListener() {} }))
    const scroll = vi.fn()
    Element.prototype.scrollIntoView = scroll
    const kpi = { total: 1, sms: 1, email: 0 }
    serveJSON([
      [
        '/api/analytics',
        {
          window: { range: '7d', from: '2026-10-22', to: '2026-10-28', tz: 'Europe/Istanbul', bucket: 'day' },
          kpis: { sent: kpi, queued: { ...kpi, pending: 1, in_flight: 0, retry: 0 }, failed: kpi, unknown: kpi },
          series: [{ at: '2026-10-28', sms: 1, email: 0 }],
          cities: { top: [], other: { cities: 0, total: 0 }, unknown: { total: 0, sms: 0, email: 0 } },
        },
      ],
      ['/api/requests', { requests: [] }],
      [
        '/api/campaigns',
        {
          campaigns: [
            {
              id: campaign.id,
              chat_id: 'c1',
              ordinal: 1,
              name: 'İstanbul sepet',
              status: 'completed',
              updated_at: '2026-10-28T09:00:00Z',
              approved_at: '2026-10-28T08:00:00Z',
              last_sent_at: '2026-10-28T09:00:00Z',
              version: 1,
              llm_provider: 'mock',
              channels: {
                sms: { excluded_consent: 0, excluded_duplicate: 0, pending: 0, in_flight: 0, retry: 0, sent: 1, failed: 0, unknown: 0, skipped_no_consent: 0 },
              },
            },
          ],
        },
      ],
    ])
    const router = createRouter({ history: createMemoryHistory(), routes })
    await router.push('/')
    await router.isReady()
    const pinia = createPinia()
    setActivePinia(pinia)
    useLiveStore()
    const w = mount(OverviewView, { global: { plugins: [router, pinia] } })
    mounted.push(w)
    await flushPromises()
    await w.get('.stat-tile').trigger('click')
    expect(scroll).toHaveBeenCalledWith({ behavior: 'auto', block: 'start' })
  })
})
