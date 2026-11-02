<script setup lang="ts">
import { t, tk } from '../../i18n'
import { computed, reactive } from 'vue'
import { RouterLink } from 'vue-router'
import type { CampaignStatus, TrackedCampaign } from '../../api/types'
import { campaignTotals, type CampaignTotals, type ChannelFilter } from '../../lib/campaigns'
import { channelName, formatCount, orderedChannels, relativeTime } from '../../lib/format'
import SegmentBar from '../charts/SegmentBar.vue'
import type { OutcomeFilter } from './CampaignTable.vue'

// Campaigns as a marketer reads them: name, state, how far sending got.
// A row opens its breakdown per channel, with who was left out at approval.
// status narrows the list to campaigns with deliveries in that outcome.
const props = withDefaults(defineProps<{ campaigns: TrackedCampaign[]; status?: OutcomeFilter; channel?: ChannelFilter }>(), {
  status: null,
  channel: '',
})

const open = reactive<Record<string, boolean>>({})
const labels: Record<CampaignStatus, string> = {
  draft: tk('Taslak'),
  approved: tk('Sırada'),
  sending: tk('Gönderiliyor'),
  completed: tk('Tamamlandı'),
  cancelled: tk('İptal'),
}
const rows = computed(() =>
  props.campaigns
    .map((c) => ({ c, tot: campaignTotals(c, props.channel) }))
    .filter(({ c, tot }) => !props.status || (c.status !== 'draft' && tot[props.status] > 0)),
)

function progress(tot: CampaignTotals): string {
  const parts = [t('{sent}/{target} gönderildi', { sent: formatCount(tot.sent), target: formatCount(tot.target) })]
  if (tot.failed) parts.push(t('{n} başarısız', { n: formatCount(tot.failed) }))
  if (tot.unknown) parts.push(t('{n} belirsiz', { n: formatCount(tot.unknown) }))
  return parts.join(' · ')
}
const segments = (tot: CampaignTotals) => [
  { key: 'sent', label: tk('Gönderildi'), value: tot.sent, tone: 'good' as const },
  { key: 'failed', label: tk('Başarısız'), value: tot.failed, tone: 'critical' as const },
  { key: 'unknown', label: tk('Belirsiz'), value: tot.unknown, tone: 'warning' as const },
  { key: 'skipped', label: tk('İzin geri alındı'), value: tot.skipped, tone: 'neutral' as const },
  { key: 'queued', label: tk('Sırada'), value: tot.queued, tone: 'track' as const },
]
function breakdown(c: TrackedCampaign) {
  return orderedChannels(Object.keys(c.channels))
    .filter((ch) => !props.channel || ch === props.channel)
    .map((ch) => {
      const tot = campaignTotals({ ...c, channels: { [ch]: c.channels[ch] } })
      const parts = [t('{n} gönderildi', { n: formatCount(tot.sent) })]
      if (tot.queued) parts.push(t('{n} sırada', { n: formatCount(tot.queued) }))
      if (tot.failed) parts.push(t('{n} başarısız', { n: formatCount(tot.failed) }))
      if (tot.unknown) parts.push(t('{n} belirsiz', { n: formatCount(tot.unknown) }))
      if (tot.skipped) parts.push(t('{n} izni geri alındı', { n: formatCount(tot.skipped) }))
      if (tot.excludedConsent) parts.push(t('{n} izin yok', { n: formatCount(tot.excludedConsent) }))
      if (tot.excludedDuplicate) parts.push(t('{n} içerik tekrarı', { n: formatCount(tot.excludedDuplicate) }))
      return { ch, tot, text: parts.join(' · ') }
    })
}
</script>

<template>
  <ul v-if="rows.length" class="campaign-list">
    <li v-for="{ c, tot } in rows" :key="c.id" class="campaign-item">
      <div class="campaign-item-head">
        <RouterLink :to="`/assistant/${c.chat_id}?campaign=${c.id}`" class="campaign-item-name">{{ c.name }}</RouterLink>
        <span class="status-chip" :class="c.status">
          <span v-if="c.status === 'sending'" class="live-dot" aria-hidden="true" />{{ t(labels[c.status]) }}
        </span>
      </div>
      <template v-if="c.status !== 'draft'">
        <SegmentBar :segments="segments(tot)" />
        <div class="campaign-item-foot">
          <span class="progress-text">{{ progress(tot) }}</span>
          <span class="campaign-item-time">{{ c.approved_at ? relativeTime(c.approved_at) : '' }}</span>
          <button type="button" class="link expand" :aria-expanded="!!open[c.id]" @click="open[c.id] = !open[c.id]">
            {{ open[c.id] ? t('Gizle') : t('Ayrıntı') }}
          </button>
        </div>
        <div v-if="open[c.id]" class="breakdown">
          <p v-for="b in breakdown(c)" :key="b.ch" class="breakdown-line">
            <i class="dot-key" :class="b.ch" aria-hidden="true" /><strong>{{ channelName(b.ch) }}</strong> {{ b.text }}
          </p>
        </div>
      </template>
      <p v-else class="campaign-item-draft">
        {{ t('Onay bekliyor · Kampanya {n}', { n: c.ordinal }) }}<template v-if="c.version"> · v{{ c.version }}</template>
      </p>
    </li>
  </ul>
  <p v-else class="list-empty">{{ status ? t('Bu sonuçta kampanya yok.') : t('Henüz kampanya yok.') }}</p>
</template>
