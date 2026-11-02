import { describe, expect, it } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import { legacyRedirect, routes } from './router'

describe('routes', () => {
  it('sends links of the first UI to the new pages', () => {
    const id = '3f6c1a2b-4d5e-4f60-8a7b-9c0d1e2f3a4b'
    expect(legacyRedirect(`#/chat/${id}`)).toBe(`/assistant/${id}`)
    expect(legacyRedirect('#/dashboard')).toBe('/')
    expect(legacyRedirect('#/')).toBeNull()
    expect(legacyRedirect('')).toBeNull()
  })

  it('names the three areas', () => {
    const router = createRouter({ history: createMemoryHistory(), routes })
    expect(router.resolve('/').name).toBe('overview')
    expect(router.resolve('/campaigns').name).toBe('campaigns')
    expect(router.resolve('/assistant').name).toBe('workspace')
    expect(router.resolve('/assistant/3f6c1a2b-4d5e-4f60-8a7b-9c0d1e2f3a4b').params.chatId).toBe('3f6c1a2b-4d5e-4f60-8a7b-9c0d1e2f3a4b')
  })

  it('resolves campaign UUIDs, not numeric internal IDs', () => {
    const router = createRouter({ history: createMemoryHistory(), routes })
    const id = '00000000-0000-4000-8000-000000000007'
    expect(router.resolve(`/campaigns/${id}`).name).toBe('campaign')
    expect(router.resolve('/campaigns/7').name).not.toBe('campaign')
  })
})
