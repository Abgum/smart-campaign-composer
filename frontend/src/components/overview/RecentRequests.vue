<script setup lang="ts">
import { t } from '../../i18n'
import { RouterLink } from 'vue-router'
import type { RecentRequest } from '../../api/types'
import { relativeTime, withoutScenario } from '../../lib/format'
import Icon from '../Icon.vue'

// What marketers asked most recently, and what came of each request: the
// campaign it created or changed, or a question back. Each opens its chat.
defineProps<{ requests: RecentRequest[] }>()


// A mock answer plays a canned demo draft whatever the request says, so its
// result is labelled as such, never as a reading of the brief.
const mockNote = () => t('Demo modeli isteği yorumlamaz; seçilen senaryoya ait hazır taslağı gösterir.')

type Result = { text: string; tone: string; title?: string }

function result(r: RecentRequest): Result {
  if (r.status === 'running') return { text: t('Hazırlanıyor'), tone: 'neutral' }
  const mock = r.llm_provider === 'mock'
  const ref = r.campaigns.at(-1)
  const more = r.campaigns.length > 1 ? ` +${r.campaigns.length - 1}` : ''
  if (ref && mock) {
    const what = ref.action === 'created' ? t('hazır demo taslağı') : t('demo düzenlemesi (v{v})', { v: ref.version })
    return { text: `Mock · ${what}: ${ref.name}${more}`, tone: 'mock', title: mockNote() }
  }
  if (ref) {
    const what = ref.action === 'created' ? t('oluşturuldu') : t('güncellendi (v{v})', { v: ref.version })
    return { text: `${ref.name} · ${what}${more}`, tone: r.outcome === 'partial' ? 'warning' : 'accent' }
  }
  const base: Result =
    r.status === 'failed' || r.outcome === 'failed'
      ? { text: t('Tamamlanamadı'), tone: 'critical' }
      : r.intent === 'clarify'
        ? { text: t('Soru soruldu'), tone: 'warning' }
        : { text: t('Yanıtlandı'), tone: 'neutral' }
  return mock ? { text: `Mock · ${base.text}`, tone: 'mock', title: mockNote() } : base
}
</script>

<template>
  <ul v-if="requests.length" class="request-list">
    <li v-for="r in requests" :key="r.turn_id" class="request-item">
      <RouterLink :to="`/assistant/${r.chat_id}`" class="request-link">
        <span class="request-icon" aria-hidden="true"><Icon name="assistant" :size="18" /></span>
        <span class="request-body">
          <span class="request-text">“{{ withoutScenario(r.text) }}”</span>
          <span class="request-meta">
            <span class="result-chip" :class="'tone-' + result(r).tone" :title="result(r).title">{{ result(r).text }}</span>
            <span class="request-time">{{ relativeTime(r.created_at) }}</span>
          </span>
        </span>
      </RouterLink>
    </li>
  </ul>
  <p v-else class="list-empty">{{ t('Henüz istek yok. Asistana bir kampanya isteği yazarak başlayın.') }}</p>
</template>
