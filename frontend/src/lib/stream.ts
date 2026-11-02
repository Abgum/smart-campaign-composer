import { onBeforeUnmount, ref, watch, type Ref } from 'vue'
import { readEvents } from '../api/sse'
import { langHeader } from '../i18n'

// useEventStream follows a GET event stream while url is set. A broken
// connection is retried; a stream the server ends on purpose (a finished
// campaign) is retried only when reconnectOnEnd is set (the dashboard).
export function useEventStream(url: Ref<string | null>, onEvent: (event: string, data: any) => void, reconnectOnEnd = false) {
  const connected = ref(false)
  let ctrl: AbortController | null = null

  async function follow(u: string, signal: AbortSignal) {
    while (!signal.aborted) {
      try {
        const resp = await fetch(u, { signal, headers: { ...langHeader(), Accept: 'text/event-stream' } })
        if (!resp.ok || !resp.body) throw new Error(`${u}: ${resp.status}`)
        connected.value = true
        for await (const m of readEvents(resp.body)) {
          try {
            onEvent(m.event, JSON.parse(m.data))
          } catch {
            // a malformed event is skipped
          }
        }
        connected.value = false
        if (!reconnectOnEnd) return
      } catch {
        connected.value = false
        if (signal.aborted) return
      }
      await new Promise((r) => setTimeout(r, 2000))
    }
  }

  watch(
    url,
    (u) => {
      ctrl?.abort()
      ctrl = null
      connected.value = false
      if (u) {
        ctrl = new AbortController()
        void follow(u, ctrl.signal)
      }
    },
    { immediate: true },
  )
  onBeforeUnmount(() => ctrl?.abort())
  return { connected }
}
