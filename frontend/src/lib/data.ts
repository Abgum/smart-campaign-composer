import { onBeforeUnmount, onMounted, ref, watch, type Ref, type WatchSource } from 'vue'
import { langHeader } from '../i18n'
import type { Analytics, RecentRequest, TrackedCampaign } from '../api/types'

// Refetches are triggered by the live version (any delivery changed) but
// at most this often, so a busy send does not hammer the API.
const minInterval = 2000

// useFetched loads JSON from url() whenever it changes, and again when
// version changes (throttled). While a refetch runs, data keeps the last
// answer: pages dim it instead of flashing a skeleton. An empty url loads
// nothing, for data a page needs only in some states.
function useFetched<T>(url: () => string, version: WatchSource<string>) {
  const data = ref<T | null>(null) as Ref<T | null>
  const loading = ref(false)
  const error = ref(false)
  let seq = 0
  let last = 0
  let timer: ReturnType<typeof setTimeout> | undefined

  async function load() {
    const mine = ++seq
    if (!url()) {
      data.value = null
      return
    }
    loading.value = true
    try {
      const resp = await fetch(url(), { headers: langHeader() })
      if (!resp.ok) throw new Error(String(resp.status))
      const body = (await resp.json()) as T
      if (mine === seq) {
        data.value = body
        error.value = false
      }
    } catch {
      if (mine === seq) error.value = true
    } finally {
      if (mine === seq) loading.value = false
      last = Date.now()
    }
  }

  watch(url, load, { immediate: true })
  watch(version, () => {
    clearTimeout(timer)
    timer = setTimeout(load, Math.max(0, minInterval - (Date.now() - last)))
  })
  onBeforeUnmount(() => clearTimeout(timer))
  return { data, loading, error, reload: load }
}

// useAnalytics follows GET /api/analytics for a query.
export function useAnalytics(query: () => string, version: WatchSource<string>) {
  return useFetched<Analytics>(() => (query() ? `/api/analytics?${query()}` : ''), version)
}

// useCampaigns follows GET /api/campaigns for a query.
export function useCampaigns(query: () => string, version: WatchSource<string>) {
  const r = useFetched<{ campaigns: TrackedCampaign[] }>(() => `/api/campaigns?${query()}`, version)
  return { ...r, list: () => r.data.value?.campaigns ?? [] }
}

// useRequests follows GET /api/requests: marketers' latest requests.
export function useRequests(limit: number, version: WatchSource<string>) {
  const r = useFetched<{ requests: RecentRequest[] }>(() => `/api/requests?limit=${limit}`, version)
  return { ...r, list: () => r.data.value?.requests ?? [] }
}

// useMedia tracks a media query (false where matchMedia is missing).
export function useMedia(query: string) {
  const matches = ref(false)
  let mq: MediaQueryList | undefined
  const update = () => (matches.value = !!mq?.matches)
  onMounted(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    mq = window.matchMedia(query)
    update()
    mq.addEventListener('change', update)
  })
  onBeforeUnmount(() => mq?.removeEventListener('change', update))
  return matches
}
