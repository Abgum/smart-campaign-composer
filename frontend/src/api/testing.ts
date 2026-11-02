// Test helpers: canned fetch answers and streams.
import type { TurnResult } from './types'

const enc = new TextEncoder()

// stream yields chunks one read at a time, then closes or breaks.
export function stream(chunks: Uint8Array[], end: 'close' | 'error' = 'close'): ReadableStream<Uint8Array> {
  let i = 0
  return new ReadableStream<Uint8Array>({
    pull(c) {
      if (i < chunks.length) {
        c.enqueue(chunks[i++])
        return
      }
      if (end === 'error') c.error(new TypeError('network error'))
      else c.close()
    },
  })
}

// split cuts bytes into reads of n bytes, so lines and multi-byte
// characters land across reads.
export function split(text: string, n: number): Uint8Array[] {
  const bytes = enc.encode(text)
  const out: Uint8Array[] = []
  for (let i = 0; i < bytes.length; i += n) out.push(bytes.slice(i, i + n))
  return out
}

export function sse(events: [string, unknown][], end: 'close' | 'error' = 'close'): Response {
  const text = events.map(([e, d], i) => `id: ${i + 1}\nevent: ${e}\ndata: ${JSON.stringify(d)}\n\n`).join('')
  return new Response(stream(split(text, 7), end), { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

export function result(turnId: string, status: TurnResult['status'], text = 'Kayıtlı yanıt'): TurnResult {
  if (status === 'running') return { turn_id: turnId, status, campaign_refs: [] }
  return {
    turn_id: turnId,
    status,
    outcome: status === 'completed' ? 'ok' : 'failed',
    assistant_message: { text, outcome: 'ok', llm_provider: 'mock', llm_model: 'mock-demo' },
    campaign_refs: [],
  }
}

export interface Call {
  method: string
  url: string
  body: any
}

type Answer = (call: Call) => Response | Promise<Response>

// fakeFetch answers by method and URL. A single answer repeats; a list is
// played in order.
export function fakeFetch(routes: [string, string, Answer | Answer[]][]) {
  const calls: Call[] = []
  const queues = routes.map(([method, url, a]) => ({ method, url, answers: Array.isArray(a) ? [...a] : [a], repeat: !Array.isArray(a) }))
  const fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const call: Call = { method: init?.method ?? 'GET', url: String(input), body: init?.body ? JSON.parse(String(init.body)) : undefined }
    calls.push(call)
    const q = queues.find((q) => q.method === call.method && q.url === call.url)
    if (!q) throw new Error(`unexpected ${call.method} ${call.url}`)
    const answer = q.repeat ? q.answers[0] : q.answers.shift()
    if (!answer) throw new Error(`no answer left for ${call.method} ${call.url}`)
    return answer(call)
  }
  return { fetch: fetch as typeof globalThis.fetch, calls }
}

export function memoryStorage(init: Record<string, string> = {}) {
  const data: Record<string, string> = { ...init }
  return {
    data,
    getItem: (k: string) => data[k] ?? null,
    setItem: (k: string, v: string) => {
      data[k] = v
    },
    removeItem: (k: string) => {
      delete data[k]
    },
  }
}
