import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { createMemoryHistory, createRouter } from 'vue-router'
import { formatNumber, langHeader, locale, setLocale, t } from './index'
import { en } from './en'
import LanguageSwitch from '../components/shell/LanguageSwitch.vue'
import AppShell from '../components/shell/AppShell.vue'
import { routes } from '../router'
import { api } from '../api/http'
import { campaignStatus, conditionText, formatCount, percent, relativeTime } from '../lib/format'
import FilterCard from '../components/overview/FilterCard.vue'
import { defaultFilters } from '../lib/filters'

afterEach(() => {
  setLocale('tr')
  vi.unstubAllGlobals()
})

describe('t', () => {
  it('speaks Turkish, the source language, with values filled in', () => {
    expect(t('Genel bakış')).toBe('Genel bakış')
    expect(t('{n} kişi', { n: 3 })).toBe('3 kişi')
  })

  it('speaks English from the catalog, plurals included', () => {
    setLocale('en')
    expect(t('Genel bakış')).toBe('Overview')
    expect(t('{n} kişi', { n: 1 })).toBe('1 person')
    expect(t('{n} kişi', { n: 3 })).toBe('3 people')
  })

  it('keeps a text it has no English for as it is', () => {
    setLocale('en')
    expect(t('Bu metnin çevirisi yok')).toBe('Bu metnin çevirisi yok')
  })

  it('has an English entry for every Turkish text it lists, and nothing empty', () => {
    for (const [tr, msg] of Object.entries(en)) {
      expect(tr.trim(), tr).not.toBe('')
      expect(typeof msg === 'function' ? msg({ n: 2 }) : msg, tr).not.toBe('')
    }
  })
})

describe('language choice', () => {
  it('remembers the choice and sets the page language', () => {
    setLocale('en')
    expect(locale.value).toBe('en')
    expect(localStorage.getItem('scc.locale')).toBe('en')
    expect(document.documentElement.lang).toBe('en')
    setLocale('tr')
    expect(document.documentElement.lang).toBe('tr')
  })

  it('writes numbers the way each language does', () => {
    expect(formatNumber(1240)).toBe('1.240')
    setLocale('en')
    expect(formatNumber(1240)).toBe('1,240')
  })

  it('asks the API for answers in the chosen language', async () => {
    setLocale('en')
    expect(langHeader()).toEqual({ 'Accept-Language': 'en' })
    let headers: HeadersInit | undefined
    vi.stubGlobal('fetch', async (_: RequestInfo | URL, init?: RequestInit) => {
      headers = init?.headers
      return new Response(JSON.stringify({ chats: [] }), { status: 200 })
    })
    await api.chats(1)
    expect(new Headers(headers).get('Accept-Language')).toBe('en')
  })
})

describe('LanguageSwitch', () => {
  it('switches between Türkçe and English, showing which is on', async () => {
    const w = mount(LanguageSwitch)
    const [tr, en] = w.findAll('button')
    expect(w.attributes('aria-label')).toBe('Dil')
    expect(tr.text()).toBe('TR')
    expect(tr.attributes('aria-pressed')).toBe('true')
    await en.trigger('click')
    expect(locale.value).toBe('en')
    expect(en.attributes('aria-pressed')).toBe('true')
    expect(w.attributes('aria-label')).toBe('Language')
  })

  it('sits in the app frame, on the main screen and on narrow screens', async () => {
    const router = createRouter({ history: createMemoryHistory(), routes })
    await router.push('/')
    await router.isReady()
    const w = mount(AppShell, {
      global: { plugins: [router] },
      props: { llm: null, workers: null, connected: true, chats: [] },
    })
    expect(w.find('.sidebar .lang-switch').exists()).toBe(true)
    expect(w.find('.topbar .lang-switch').exists()).toBe(true)
    await w.get('.sidebar .lang-switch button[lang="en"]').trigger('click')
    await nextTick()
    expect(w.get('.nav-item[aria-current="page"]').text()).toBe('Overview')
  })
})

// Every text the source passes to t() or tk() has an English entry. The
// source is read as text, so a new string without a translation fails here.
const sources = import.meta.glob(['../**/*.vue', '../**/*.ts', '!../**/*.spec.ts', '!../i18n/**'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

describe('catalog', () => {
  it('has English for every text in the source', () => {
    const keys = new Set<string>()
    const call = /\bt(?:k)?\(\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")/g
    for (const code of Object.values(sources)) {
      for (const m of code.matchAll(call)) keys.add((m[1] ?? m[2]).replace(/\\(['"])/g, '$1'))
    }
    expect(keys.size).toBeGreaterThan(300)
    expect([...keys].filter((k) => !(k in en))).toEqual([])
  })
})

describe('formats in English', () => {
  it('names statuses, times, shares and audience rules in English', () => {
    setLocale('en')
    expect(campaignStatus.sending).toBe('Sending')
    expect(formatCount(1240)).toBe('1,240')
    expect(percent(68, 100)).toBe('68%')
    expect(relativeTime('2026-10-30T10:00:00Z', Date.parse('2026-10-30T12:00:00Z'))).toBe('2 h ago')
    expect(relativeTime('2026-10-29T10:00:00Z', Date.parse('2026-10-30T12:00:00Z'))).toBe('1 day ago')
    expect(conditionText({ kind: 'event', event_type: 'cart_add', within_days: 30, not_followed_by: 'purchase' })).toBe(
      'Adding to cart in the last 30 days, then no purchase',
    )
    expect(conditionText({ kind: 'event', event_type: 'purchase', within_days: 1, negate: true })).toBe('No purchase in the last 1 day')
    expect(conditionText({ kind: 'location', operator: 'in', places: [{ level: 'city', name: 'İzmir' }] })).toBe('Location: İzmir (city)')
  })

  it('keeps the Turkish sentences as they were', () => {
    expect(campaignStatus.sending).toBe('Gönderiliyor')
    expect(percent(68, 100)).toBe('%68')
    expect(conditionText({ kind: 'event', event_type: 'cart_add', within_days: 30, not_followed_by: 'purchase' })).toBe(
      'Son 30 günde sepete ekleme, ardından satın alma yok',
    )
    expect(conditionText({ kind: 'event', event_type: 'purchase', within_days: 30, negate: true })).toBe('Son 30 günde satın alma yok')
  })
})

describe('filters in English', () => {
  it('names the ranges and channels in the chosen language', async () => {
    const w = mount(FilterCard, { props: { filters: defaultFilters, campaigns: [] } })
    expect(w.findAll('button[data-range]').map((b) => b.text())).toEqual(['Bugün', '7 gün', '30 gün', '90 gün', 'Tümü'])
    setLocale('en')
    await nextTick()
    expect(w.findAll('button[data-range]').map((b) => b.text())).toEqual(['Today', '7 days', '30 days', '90 days', 'All'])
    expect(w.findAll('button[data-channel]').map((b) => b.text())).toEqual(['All', 'SMS', 'E-mail'])
  })
})
