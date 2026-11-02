import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { routes } from '../../router'
import AppShell from './AppShell.vue'

async function shellAt(path: string, props: Record<string, unknown> = {}) {
  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(path)
  await router.isReady()
  return mount(AppShell, {
    global: { plugins: [router] },
    props: { llm: { provider: 'mock', model: 'mock-demo' }, workers: { alive: 1, total: 2 }, connected: true, chats: [], ...props },
  })
}

describe('AppShell', () => {
  it('opens the campaign creation choice from the global action', async () => {
    const w = await shellAt('/campaigns')
    expect(w.get('.sidebar .new-campaign').attributes('href')).toBe('/campaigns/new')
  })
  it('marks the current area in every navigation', async () => {
    const w = await shellAt('/campaigns')
    const current = w.findAll('[aria-current="page"]').map((a) => a.text())
    expect(current.length).toBeGreaterThan(0)
    expect(current.every((t) => t.includes('Kampanyalar'))).toBe(true)
  })

  it('treats any chat as the assistant area', async () => {
    const w = await shellAt('/assistant/3f6c1a2b-4d5e-4f60-8a7b-9c0d1e2f3a4b')
    const current = w.findAll('[aria-current="page"]')
    expect(current.length).toBeGreaterThan(0)
    expect(current.every((a) => a.text().includes('Asistan'))).toBe(true)
  })

  it('shows which model answers and, plainly, whether sending runs', async () => {
    const w = await shellAt('/')
    expect(w.text()).toContain('MOCK · mock-demo')
    expect(w.text()).toContain('Gönderim servisi çalışıyor')
    expect(w.text()).not.toContain('worker')
  })

  it('warns when sending does not run', async () => {
    const w = await shellAt('/', { workers: { alive: 0, total: 1 } })
    expect(w.find('.status-warning').text()).toContain('Gönderim servisi yanıt vermiyor')
  })

  it('shows the logo with the name, in the sidebar and the top bar', async () => {
    const w = await shellAt('/')
    for (const where of ['.sidebar .brand', '.topbar']) {
      expect(w.get(where).find('svg.logo').exists()).toBe(true)
      expect(w.get(where).text()).toContain('Smart Campaign Composer')
    }
  })

  it('lets the assistant fill the window, and only the assistant', async () => {
    expect((await shellAt('/assistant/c1')).get('.shell').classes()).toContain('fill')
    expect((await shellAt('/')).get('.shell').classes()).not.toContain('fill')
  })

  it('lists recent chats as links to the assistant', async () => {
    const w = await shellAt('/', { chats: [{ id: 'c1', title: 'İstanbul sepet hatırlatma', updated_at: '2026-10-27T10:00:00Z' }] })
    const link = w.findAll('a').find((a) => a.text().includes('İstanbul sepet hatırlatma'))
    expect(link?.attributes('href')).toBe('/assistant/c1')
  })
})
