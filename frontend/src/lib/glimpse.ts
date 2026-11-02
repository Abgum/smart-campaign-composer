// glimpse reads the readable parts of a draft's tool input while it streams
// (partial JSON), for the live preview only. Nothing here is trusted or
// stored: the saved draft is read back from the API after the turn.
export interface Glimpse {
  name?: string
  sms?: string
  subject?: string
  body?: string
}

function field(partial: string, re: RegExp): string | undefined {
  const m = partial.match(re)
  if (!m) return undefined
  let raw = m[1]
  if (raw.endsWith('\\') && !raw.endsWith('\\\\')) raw = raw.slice(0, -1) // a cut escape
  raw = raw.replace(/\\u[0-9a-fA-F]{0,3}$/, '')
  try {
    return JSON.parse(`"${raw}"`)
  } catch {
    return raw
  }
}

export function glimpse(partial: string): Glimpse {
  return {
    name: field(partial, /"name"\s*:\s*"((?:[^"\\]|\\.)*)/),
    sms: field(partial, /"sms"\s*:\s*\{\s*"text"\s*:\s*"((?:[^"\\]|\\.)*)/),
    subject: field(partial, /"subject"\s*:\s*"((?:[^"\\]|\\.)*)/),
    body: field(partial, /"body"\s*:\s*"((?:[^"\\]|\\.)*)/),
  }
}
