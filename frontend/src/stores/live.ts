import { langHeader } from '../i18n'
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { readEvents } from '../api/sse'
import type { Dashboard } from '../api/types'

// The live state of sending, from one /api/dashboard/stream connection
// shared by the whole app: campaigns in progress, the queue, channel limits
// and pauses, workers, and a version that changes whenever any delivery or
// campaign does (pages refetch their analytics on it).
export const useLiveStore = defineStore('live', () => {
  const board = ref<Dashboard | null>(null)
  const connected = ref(false)
  let started = false

  async function follow() {
    for (;;) {
      try {
        const resp = await fetch('/api/dashboard/stream', { headers: { ...langHeader(), Accept: 'text/event-stream' } })
        if (!resp.ok || !resp.body) throw new Error(String(resp.status))
        connected.value = true
        for await (const m of readEvents(resp.body)) {
          if (m.event !== 'dashboard') continue
          try {
            board.value = JSON.parse(m.data) as Dashboard
          } catch {
            // a malformed event is skipped
          }
        }
      } catch {
        // retried below
      }
      connected.value = false
      await new Promise((r) => setTimeout(r, 2000))
    }
  }

  function start() {
    if (started) return
    started = true
    void follow()
  }

  const workers = computed(() =>
    board.value ? { alive: board.value.workers.filter((w) => w.alive).length, total: board.value.workers.length } : null,
  )
  const version = computed(() => board.value?.version ?? '')

  return { board, connected, workers, version, start }
})
