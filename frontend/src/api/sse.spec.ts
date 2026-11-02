// @vitest-environment node
import { describe, expect, it } from 'vitest'
import { readEvents, type SSEMessage } from './sse'
import { split, stream } from './testing'

async function collect(body: ReadableStream<Uint8Array>): Promise<SSEMessage[]> {
  const out: SSEMessage[] = []
  for await (const m of readEvents(body)) out.push(m)
  return out
}

describe('readEvents', () => {
  it('parses events split across reads, multi-byte text included', async () => {
    const text =
      'id: 1\nevent: assistant.delta\ndata: {"text":"Sepetin ğüşiöç"}\n\n' + ': yorum\n\n' + 'id: 2\nevent: done\ndata: {"outcome":"ok"}\n\n'
    expect(await collect(stream(split(text, 3)))).toEqual([
      { event: 'assistant.delta', data: '{"text":"Sepetin ğüşiöç"}' },
      { event: 'done', data: '{"outcome":"ok"}' },
    ])
  })

  it('accepts CRLF line ends and joins multi-line data', async () => {
    const text = 'event: x\r\ndata: a\r\ndata: b\r\n\r\n'
    expect(await collect(stream(split(text, 4)))).toEqual([{ event: 'x', data: 'a\nb' }])
  })

  it('throws when the stream breaks', async () => {
    const body = stream(split('event: x\ndata: 1\n\nevent: y\nda', 5), 'error')
    const got: SSEMessage[] = []
    await expect(async () => {
      for await (const m of readEvents(body)) got.push(m)
    }).rejects.toThrow()
    expect(got).toEqual([{ event: 'x', data: '1' }])
  })
})
