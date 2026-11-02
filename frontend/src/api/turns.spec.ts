// @vitest-environment node
import { describe, expect, it, vi } from 'vitest'
import { TurnClient, pendingTurnKey, type KeyValueStore, type TurnEvent } from './turns'
import { fakeFetch, json, memoryStorage, result, sse } from './testing'

function setup(routes: Parameters<typeof fakeFetch>[0], storage: KeyValueStore = memoryStorage()) {
  const { fetch, calls } = fakeFetch(routes)
  const events: TurnEvent[] = []
  const sleeps: number[] = []
  let n = 0
  const client = new TurnClient((e) => events.push(e), {
    fetch,
    storage,
    sleep: async (ms) => {
      sleeps.push(ms)
    },
    newId: () => `00000000-0000-4000-8000-00000000000${++n}`,
  })
  return { client, events, calls, sleeps, storage }
}

const types = (events: TurnEvent[]) => events.map((e) => e.type)
const posts = (calls: { method: string; body: any }[]) => calls.filter((c) => c.method === 'POST')
const resultText = (events: TurnEvent[]) => {
  const last = events.at(-1)
  return last?.type === 'result' ? last.result.assistant_message?.text : undefined
}

const started = ['turn.started', { turn_id: 't1' }] as [string, unknown]
const delta = (text: string) => ['assistant.delta', { text }] as [string, unknown]
const done = ['done', { turn_id: 't1', outcome: 'ok' }] as [string, unknown]
const campaignId = '00000000-0000-4000-8000-000000000003'

describe('TurnClient', () => {
  it('shows the stored result after done, not the streamed preview', async () => {
    const { client, events, calls } = setup([
      ['POST', '/api/chats/c1/turns', () => sse([started, delta('Önizleme '), delta('metni'), done])],
      ['GET', '/api/turns/t1', () => json(200, result('t1', 'completed', 'Kayıtlı yanıt'))],
    ])
    await client.send('c1', 'selam', campaignId)

    expect(types(events)).toEqual(['started', 'live', 'live', 'result'])
    expect(events[1]).toEqual({ type: 'live', event: 'assistant.delta', data: { text: 'Önizleme ' } })
    expect(resultText(events)).toBe('Kayıtlı yanıt')
    expect(posts(calls)[0].body).toEqual({ client_turn_id: '00000000-0000-4000-8000-000000000001', text: 'selam', focus_campaign_id: campaignId })
  })

  it('drops the preview when the connection breaks, then polls the stored result with backoff', async () => {
    const { client, events, sleeps } = setup([
      ['POST', '/api/chats/c1/turns', () => sse([started, delta('Mer')], 'error')],
      ['GET', '/api/turns/t1', [() => json(200, result('t1', 'running')), () => json(200, result('t1', 'running')), () => json(200, result('t1', 'completed'))]],
    ])
    await client.send('c1', 'selam', null)

    expect(types(events)).toEqual(['started', 'live', 'preview-reset', 'waiting', 'result'])
    expect(events[2]).toMatchObject({ type: 'preview-reset', reason: 'disconnected' })
    expect(sleeps).toEqual([1000, 2000, 5000])
    expect(resultText(events)).toBe('Kayıtlı yanıt')
  })

  it('repeats a POST whose answer never came with the same client_turn_id', async () => {
    const { client, events, calls } = setup([
      ['POST', '/api/chats/c1/turns', [() => Promise.reject(new TypeError('Failed to fetch')), () => json(202, { turn_id: 't1', status: 'running' })]],
      ['GET', '/api/turns/t1', () => json(200, result('t1', 'completed'))],
    ])
    await client.send('c1', 'selam', null)

    const [first, second] = posts(calls)
    expect(second.body.client_turn_id).toBe(first.body.client_turn_id)
    expect(types(events)).toEqual(['waiting', 'result'])
  })

  it('repeats the POST when the stream breaks before the turn is known', async () => {
    const { client, events, calls } = setup([
      ['POST', '/api/chats/c1/turns', [() => sse([], 'error'), () => json(200, result('t1', 'completed'))]],
    ])
    await client.send('c1', 'selam', null)

    const [first, second] = posts(calls)
    expect(second.body.client_turn_id).toBe(first.body.client_turn_id)
    expect(types(events)).toEqual(['result'])
  })

  it('takes a 200 answer as the stored result: no stream, no polling', async () => {
    const { client, events, calls } = setup([['POST', '/api/chats/c1/turns', () => json(200, result('t1', 'completed'))]])
    await client.send('c1', 'selam', null)

    expect(types(events)).toEqual(['result'])
    expect(calls.filter((c) => c.method === 'GET')).toHaveLength(0)
  })

  it('on resync lagged drops the preview and still shows the stored result after done', async () => {
    const lagged = ['resync', { turn_id: 't1', reason: 'lagged', result_url: '/api/turns/t1', message: 'yavaş' }] as [string, unknown]
    const { client, events } = setup([
      ['POST', '/api/chats/c1/turns', () => sse([started, delta('Mer'), lagged, done])],
      ['GET', '/api/turns/t1', () => json(200, result('t1', 'completed'))],
    ])
    await client.send('c1', 'selam', null)

    expect(types(events)).toEqual(['started', 'live', 'preview-reset', 'result'])
    expect(events[2]).toMatchObject({ reason: 'lagged', message: 'yavaş' })
  })

  it('on resync result_pending polls until the turn is no longer running', async () => {
    const pending = ['resync', { turn_id: 't1', reason: 'result_pending', result_url: '/api/turns/t1' }] as [string, unknown]
    const failure = ['error', { code: 'persist_failed', message: 'kaydedilemedi' }] as [string, unknown]
    const { client, events } = setup([
      ['POST', '/api/chats/c1/turns', () => sse([started, failure, pending])],
      ['GET', '/api/turns/t1', [() => json(200, result('t1', 'running')), () => json(200, result('t1', 'failed'))]],
    ])
    await client.send('c1', 'selam', null)

    expect(types(events)).toEqual(['started', 'live', 'preview-reset', 'waiting', 'result'])
    expect(events[2]).toMatchObject({ reason: 'result_pending' })
  })

  it('reports a refused turn and forgets it', async () => {
    const { client, events, storage } = setup([
      ['POST', '/api/chats/c1/turns', () => json(409, { error: 'turn_in_progress', message: 'bu sohbette çalışan bir istek var' })],
    ])
    await client.send('c1', 'selam', null)

    expect(events).toEqual([{ type: 'failed', code: 'turn_in_progress', message: 'bu sohbette çalışan bir istek var', status: 409 }])
    expect(storage.getItem(pendingTurnKey('c1'))).toBeNull()
  })

  // A 5xx or an answer that is not the API's JSON (a proxy's 502 page) does
  // not say the turn was not made: the server may have started it. The POST
  // is repeated with the same id, which never runs a turn twice.
  it('repeats a POST answered by a server error with the same client_turn_id', async () => {
    const { client, events, calls, storage } = setup([
      ['POST', '/api/chats/c1/turns', [
        () => json(500, { error: 'internal_error', message: 'iç hata' }),
        () => new Response('<html>502 Bad Gateway</html>', { status: 502, headers: { 'Content-Type': 'text/html' } }),
        () => json(202, { turn_id: 't1', status: 'running' }),
      ]],
      ['GET', '/api/turns/t1', () => json(200, result('t1', 'completed'))],
    ])
    await client.send('c1', 'selam', null)

    const ids = posts(calls).map((c) => c.body.client_turn_id)
    expect(ids).toHaveLength(3)
    expect(new Set(ids).size).toBe(1)
    expect(types(events)).toEqual(['waiting', 'result'])
    expect(storage.getItem(pendingTurnKey('c1'))).toBeNull()
  })

  it('keeps the turn pending when server errors last, so "Yeniden bağlan" repeats it', async () => {
    const { client, events, storage } = setup([['POST', '/api/chats/c1/turns', () => json(503, { error: 'unavailable', message: 'bakımda' })]])
    await client.send('c1', 'selam', null)

    expect(events.at(-1)).toMatchObject({ type: 'failed', code: 'network' })
    expect(storage.getItem(pendingTurnKey('c1'))).not.toBeNull()
  })

  it('keeps a turn pending until its result arrives, so a reload can resume it with the same id', async () => {
    const storage = memoryStorage({ [pendingTurnKey('c1')]: JSON.stringify({ clientTurnId: 'abc', text: 'selam', focusCampaignId: null }) })
    const { client, events, calls } = setup([['POST', '/api/chats/c1/turns', () => json(200, result('t1', 'completed'))]], storage)

    expect(await client.resume('c1', null)).toBe(true)
    expect(posts(calls)[0].body).toEqual({ client_turn_id: 'abc', text: 'selam', focus_campaign_id: null })
    expect(types(events)).toEqual(['result'])
    expect(storage.getItem(pendingTurnKey('c1'))).toBeNull()
  })

  it('on reload polls a running turn the chat reports', async () => {
    const { client, events } = setup([
      ['GET', '/api/turns/t9', [() => json(200, result('t9', 'running')), () => json(200, result('t9', 'completed'))]],
    ])
    expect(await client.resume('c1', 't9')).toBe(true)
    expect(types(events)).toEqual(['waiting', 'result'])
  })

  it('makes v4 ids where crypto.randomUUID is missing (plain http, not localhost)', async () => {
    const real = globalThis.crypto
    vi.stubGlobal('crypto', { getRandomValues: real.getRandomValues.bind(real) })
    try {
      const { fetch, calls } = fakeFetch([['POST', '/api/chats/c1/turns', () => json(200, result('t1', 'completed'))]])
      const events: TurnEvent[] = []
      await new TurnClient((e) => events.push(e), { fetch, storage: null }).send('c1', 'selam', null)
      expect(types(events)).toEqual(['result'])
      expect(posts(calls)[0].body.client_turn_id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('never leaves the UI waiting: an unexpected error is reported', async () => {
    const events: TurnEvent[] = []
    const client = new TurnClient((e) => events.push(e), {
      storage: null,
      newId: () => {
        throw new Error('boom')
      },
    })
    await client.send('c1', 'selam', null)
    expect(types(events)).toEqual(['failed'])
  })

  it('works when storage throws (private mode)', async () => {
    const broken: KeyValueStore = {
      getItem: () => {
        throw new Error('denied')
      },
      setItem: () => {
        throw new Error('denied')
      },
      removeItem: () => {
        throw new Error('denied')
      },
    }
    const { client, events } = setup([['POST', '/api/chats/c1/turns', () => json(200, result('t1', 'completed'))]], broken)
    await client.send('c1', 'selam', null)
    expect(types(events)).toEqual(['result'])
  })
})
