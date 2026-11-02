import { langHeader, tk } from '../i18n'
import { readEvents } from './sse'
import type { TurnResult } from './types'

// The turn client sends a chat message and reports the turn's outcome.
//
// Contract with the API (backend internal/httpapi/stream.go):
//   - A new turn answers with an event stream. Its live events are only a
//     preview; "done" means the result is stored, and the stored result
//     (GET /api/turns/{id}) is what the UI keeps.
//   - "resync lagged": live events stopped; drop the preview, wait for done.
//   - "resync result_pending", or a stream that breaks: drop the preview and
//     poll the stored result until the turn is no longer running.
//   - Every message carries a client_turn_id. A POST whose answer never
//     came, or came as a server error, is repeated with the same id: the
//     server never runs a turn twice and answers 202 (running) or 200 (the
//     stored result) instead.
//   - Every event is reported with the chat it belongs to: a turn goes on
//     when the user opens another chat, and only that turn's chat shows it.
//   - The turn stays pending in session storage until its result arrives,
//     so a reload resumes it with the same id.

// What the client reports, in order. "live" events are a preview only;
// "result" carries the stored outcome.
export type TurnEvent =
  | { type: 'started'; turnId: string }
  | { type: 'live'; event: string; data: any }
  | { type: 'preview-reset'; reason: 'lagged' | 'disconnected' | 'result_pending'; message?: string }
  | { type: 'waiting'; turnId: string }
  | { type: 'result'; result: TurnResult }
  | { type: 'failed'; code: string; message: string; status?: number }

export interface KeyValueStore {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export interface TurnClientOptions {
  fetch?: typeof fetch
  sleep?: (ms: number) => Promise<void>
  storage?: KeyValueStore | null // undefined: sessionStorage; null: none
  newId?: () => string
}

const POLL_DELAYS = [1000, 2000, 5000] // then every 5s
const POST_RETRY_DELAYS = [1000, 2000, 5000]
const MAX_POST_ATTEMPTS = 5
const MAX_POLLS = 150 // about 12 minutes at 5s, far beyond a turn's timeout

interface Pending {
  clientTurnId: string
  text: string
  focusCampaignId: string | null
  turnId?: string
}

const delay = (delays: number[], i: number) => delays[Math.min(i, delays.length - 1)]

export class TurnClient {
  private readonly fetch: typeof fetch
  private readonly sleep: (ms: number) => Promise<void>
  private readonly storage: KeyValueStore | null
  private readonly newId: () => string

  constructor(
    private readonly emit: (e: TurnEvent, chatId: string) => void,
    opts: TurnClientOptions = {},
  ) {
    this.fetch = opts.fetch ?? ((input, init) => globalThis.fetch(input, init))
    this.sleep = opts.sleep ?? ((ms) => new Promise((resolve) => setTimeout(resolve, ms)))
    this.storage = opts.storage === undefined ? sessionStore() : opts.storage
    this.newId = opts.newId ?? uuidv4
  }

  // send starts a turn and reports it until its stored result arrives. It
  // never throws: whatever goes wrong ends in a "failed" event, so the UI
  // is never left waiting.
  async send(chatId: string, text: string, focusCampaignId: string | null): Promise<void> {
    try {
      const p: Pending = { clientTurnId: this.newId(), text, focusCampaignId }
      this.save(chatId, p)
      await this.run(chatId, p)
    } catch {
      this.unexpected(chatId)
    }
  }

  // resume picks up after a reload: a pending turn is repeated with its id
  // (the server answers from the stored turn if it has one), a running turn
  // is polled. It reports whether there was anything to resume.
  async resume(chatId: string, runningTurnId: string | null): Promise<boolean> {
    try {
      const p = this.load(chatId)
      if (p) {
        await this.run(chatId, p)
        return true
      }
      if (runningTurnId) {
        this.emit({ type: 'waiting', turnId: runningTurnId }, chatId)
        this.finish(chatId, await this.poll(runningTurnId))
        return true
      }
    } catch {
      this.unexpected(chatId)
    }
    return false
  }

  private unexpected(chatId: string) {
    this.fail(chatId, 'client_error', tk('Beklenmeyen bir hata oluştu; sayfayı yenileyip tekrar deneyin.'))
  }

  private async run(chatId: string, p: Pending): Promise<void> {
    for (let attempt = 0; ; attempt++) {
      let resp: Response | null = null
      try {
        resp = await this.fetch(`/api/chats/${chatId}/turns`, {
          method: 'POST',
          headers: { ...langHeader(), 'Content-Type': 'application/json' },
          body: JSON.stringify({ client_turn_id: p.clientTurnId, text: p.text, focus_campaign_id: p.focusCampaignId }),
        })
      } catch {
        // No answer: the server may or may not have the turn. Repeating
        // with the same id is safe either way.
      }
      if (resp?.ok && (resp.headers.get('Content-Type') ?? '').startsWith('text/event-stream') && resp.body) {
        if (await this.consume(chatId, p, resp.body)) return
        // The stream broke before the turn was announced: ask again.
      } else if (resp) {
        const body = await resp.json().catch(() => null)
        if (resp.status === 202 && body?.turn_id) {
          this.emit({ type: 'waiting', turnId: body.turn_id }, chatId)
          this.finish(chatId, await this.poll(body.turn_id))
          return
        }
        if (resp.ok && body?.turn_id) {
          this.finish(chatId, body as TurnResult)
          return
        }
        // A server error, or an answer that is not the API's JSON (a
        // proxy's error page), does not say the turn was not made: ask
        // again with the same id, as when no answer came.
        if (resp.status < 500 && body !== null) {
          this.fail(chatId, body?.error ?? `http_${resp.status}`, body?.message ?? tk('İstek başarısız oldu.'), resp.status)
          return
        }
      }
      if (attempt + 1 >= MAX_POST_ATTEMPTS) {
        this.fail(chatId, 'network', tk('Sunucuya ulaşılamadı. Bağlantı gelince "Yeniden bağlan" ile aynı isteği sürdürebilirsiniz.'))
        return
      }
      await this.sleep(delay(POST_RETRY_DELAYS, attempt))
    }
  }

  // consume reads a turn's stream to its end. It reports false when the
  // stream broke before the turn was announced (nothing is known of it).
  private async consume(chatId: string, p: Pending, body: ReadableStream<Uint8Array>): Promise<boolean> {
    let turnId: string | null = null
    let done = false
    let lagged = false
    let resultPending = false
    try {
      for await (const m of readEvents(body)) {
        const data = parseJSON(m.data)
        switch (m.event) {
          case 'turn.started':
            turnId = String(data.turn_id)
            this.save(chatId, { ...p, turnId })
            this.emit({ type: 'started', turnId }, chatId)
            break
          case 'resync':
            if (data.reason === 'lagged') {
              lagged = true
              this.emit({ type: 'preview-reset', reason: 'lagged', message: data.message }, chatId)
            } else {
              resultPending = true
            }
            break
          case 'done':
            done = true
            break
          default:
            if (!lagged || m.event === 'error') this.emit({ type: 'live', event: m.event, data }, chatId)
        }
      }
    } catch {
      // The connection broke; what was stored is fetched below.
    }
    if (turnId === null) return false
    if (done) {
      this.finish(chatId, await this.poll(turnId, true))
      return true
    }
    this.emit({ type: 'preview-reset', reason: resultPending ? 'result_pending' : 'disconnected' }, chatId)
    this.emit({ type: 'waiting', turnId }, chatId)
    this.finish(chatId, await this.poll(turnId))
    return true
  }

  // poll fetches the stored result until the turn is no longer running
  // (immediate: ask once before waiting, when done says it is stored).
  private async poll(turnId: string, immediate = false): Promise<TurnResult | null> {
    for (let i = 0; i < MAX_POLLS; i++) {
      if (!immediate || i > 0) await this.sleep(delay(POLL_DELAYS, immediate ? i - 1 : i))
      try {
        const resp = await this.fetch(`/api/turns/${turnId}`, { headers: langHeader() })
        if (resp.status === 404) return null
        if (resp.ok) {
          const r = (await resp.json()) as TurnResult
          if (r.status !== 'running') return r
        }
      } catch {
        // Still offline: keep trying.
      }
    }
    return null
  }

  private finish(chatId: string, result: TurnResult | null) {
    if (!result) {
      this.fail(chatId, 'result_unavailable', tk('Tur sonucu alınamadı; sayfayı yenileyin.'))
      return
    }
    this.clear(chatId)
    this.emit({ type: 'result', result }, chatId)
  }

  // fail reports a turn that did not run or whose result cannot be read. A
  // turn refused by the server is forgotten; one that may not have reached
  // it stays pending, to be repeated with its id.
  private fail(chatId: string, code: string, message: string, status?: number) {
    if (code !== 'network') this.clear(chatId)
    this.emit({ type: 'failed', code, message, status }, chatId)
  }

  private save(chatId: string, p: Pending) {
    try {
      this.storage?.setItem(pendingTurnKey(chatId), JSON.stringify(p))
    } catch {
      // Storage unavailable (private mode): the turn just cannot survive a reload.
    }
  }

  private load(chatId: string): Pending | null {
    try {
      const raw = this.storage?.getItem(pendingTurnKey(chatId))
      return raw ? (JSON.parse(raw) as Pending) : null
    } catch {
      return null
    }
  }

  private clear(chatId: string) {
    try {
      this.storage?.removeItem(pendingTurnKey(chatId))
    } catch {
      // see save
    }
  }
}

// pendingTurnKey is where a chat's unfinished turn is kept across reloads.
export function pendingTurnKey(chatId: string): string {
  return `scc.pendingTurn.${chatId}`
}

// uuidv4 makes a client_turn_id. crypto.randomUUID exists only in secure
// contexts (HTTPS, localhost); getRandomValues exists everywhere, so the
// app also works over plain HTTP from another host.
export function uuidv4(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const b = crypto.getRandomValues(new Uint8Array(16))
  b[6] = (b[6] & 0x0f) | 0x40
  b[8] = (b[8] & 0x3f) | 0x80
  const h = [...b].map((x) => x.toString(16).padStart(2, '0')).join('')
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}

function sessionStore(): KeyValueStore | null {
  try {
    return globalThis.sessionStorage ?? null
  } catch {
    return null
  }
}

function parseJSON(s: string): any {
  try {
    return JSON.parse(s)
  } catch {
    return {}
  }
}
