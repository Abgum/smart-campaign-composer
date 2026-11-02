<script lang="ts">
// Campaigns with their delivery outcome; a row opens its breakdown by
// channel: the outcome bar and who was left out at approval. status narrows
// the list to campaigns with deliveries in that outcome (a KPI picks it).
export type OutcomeFilter = 'sent' | 'queued' | 'failed' | 'unknown' | null
</script>

<script setup lang="ts">
import { t } from '../../i18n'
import { computed, reactive } from 'vue'
import { RouterLink } from 'vue-router'
import type { TrackedCampaign } from '../../api/types'
import { campaignTotals, type ChannelFilter } from '../../lib/campaigns'
import { tk } from '../../i18n'
import { channelName, formatCount, orderedChannels, relativeTime } from '../../lib/format'
import { statusLabel } from '../../lib/schedule'
import Icon from '../Icon.vue'
import SegmentBar from '../charts/SegmentBar.vue'

const props = withDefaults(defineProps<{ campaigns: TrackedCampaign[]; status?: OutcomeFilter; channel?: ChannelFilter }>(), {
  status: null,
  channel: '',
})

const open = reactive<Record<string, boolean>>({})
const rows = computed(() =>
  props.campaigns
    .map((c) => ({ c, tot: campaignTotals(c, props.channel) }))
    .filter(({ tot }) => !props.status || tot[props.status] > 0),
)
const channelsOf = (c: TrackedCampaign) =>
  orderedChannels(Object.keys(c.channels))
    .filter((ch) => !props.channel || ch === props.channel)
    .map((ch) => ({ ch, tot: campaignTotals({ ...c, channels: { [ch]: c.channels[ch] } }) }))
const segments = (tot: ReturnType<typeof campaignTotals>) => [
  { key: 'sent', label: tk('Gönderildi'), value: tot.sent, tone: 'good' as const },
  { key: 'failed', label: tk('Başarısız'), value: tot.failed, tone: 'critical' as const },
  { key: 'unknown', label: tk('Belirsiz'), value: tot.unknown, tone: 'warning' as const },
  { key: 'skipped', label: tk('İzin geri alındı'), value: tot.skipped, tone: 'neutral' as const },
  { key: 'queued', label: tk('Sırada'), value: tot.queued, tone: 'track' as const },
]
</script>

<template>
  <table class="campaign-table">
    <thead>
      <tr>
        <th scope="col">{{ t('Kampanya') }}</th>
        <th scope="col">{{ t('Hedef') }}</th>
        <th scope="col">{{ t('Gönderildi') }}</th>
        <th scope="col">{{ t('Kuyrukta') }}</th>
        <th scope="col">{{ t('Başarısız') }}</th>
        <th scope="col">{{ t('Belirsiz') }}</th>
        <th scope="col">{{ t('Elenen') }}</th>
        <th scope="col"><span class="sr-only">{{ t('Kırılım') }}</span></th>
      </tr>
    </thead>
    <tbody>
      <tr v-if="!rows.length">
        <td colspan="8" class="table-empty">{{ t('Bu durumda kampanya yok.') }}</td>
      </tr>
      <template v-for="{ c, tot } in rows" :key="c.id">
        <tr class="campaign-row" :class="{ open: open[c.id] }">
          <td :data-label="t('Kampanya')">
            <RouterLink :to="`/campaigns/${c.id}`" class="campaign-name">{{ c.name }}</RouterLink>
            <span class="campaign-sub">
              {{ t('Kampanya {n}', { n: c.ordinal }) }} · <span class="status" :class="c.status">{{ statusLabel(c.status, c.scheduled_at) }}</span>
              <template v-if="c.approved_at"> · {{ relativeTime(c.approved_at) }}</template>
            </span>
          </td>
          <td :data-label="t('Hedef')" :aria-label="c.status === 'draft' ? t('Hedef onayda belirlenir') : undefined">
            {{ c.status === 'draft' ? '—' : formatCount(tot.target) }}
          </td>
          <td :data-label="t('Gönderildi')">{{ formatCount(tot.sent) }}</td>
          <td :data-label="t('Kuyrukta')">{{ formatCount(tot.queued) }}</td>
          <td :data-label="t('Başarısız')" :class="{ 'critical-text': tot.failed }">{{ formatCount(tot.failed) }}</td>
          <td :data-label="t('Belirsiz')" :class="{ 'warning-text': tot.unknown }">{{ formatCount(tot.unknown) }}</td>
          <td :data-label="t('Elenen')">{{ formatCount(tot.excludedConsent + tot.excludedDuplicate) }}</td>
          <td class="expand-cell">
            <button
              type="button"
              class="icon-button expand"
              :aria-expanded="!!open[c.id]"
              :aria-label="open[c.id] ? t('{name}: kırılımı gizle', { name: c.name }) : t('{name}: kırılımı göster', { name: c.name })"
              @click="open[c.id] = !open[c.id]"
            >
              <Icon name="chevron" />
            </button>
          </td>
        </tr>
        <tr v-if="open[c.id]" class="breakdown-row">
          <td colspan="8">
            <div class="breakdown">
              <div v-for="b in channelsOf(c)" :key="b.ch" class="breakdown-channel">
                <span class="breakdown-label"><i class="key" :class="b.ch" aria-hidden="true" />{{ channelName(b.ch) }}</span>
                <SegmentBar :segments="segments(b.tot)" />
                <span class="breakdown-numbers">
                  {{ t('{sent} gönderildi · {failed} başarısız · {unknown} belirsiz · {queued} sırada · {consent} izin yok · {duplicate} içerik tekrarı', {
                    sent: formatCount(b.tot.sent),
                    failed: formatCount(b.tot.failed),
                    unknown: formatCount(b.tot.unknown),
                    queued: formatCount(b.tot.queued),
                    consent: formatCount(b.tot.excludedConsent),
                    duplicate: formatCount(b.tot.excludedDuplicate),
                  }) }}
                </span>
              </div>
              <RouterLink :to="`/assistant/${c.chat_id}?campaign=${c.id}`" class="breakdown-link">{{ t('Asistanda aç →') }}</RouterLink>
            </div>
          </td>
        </tr>
      </template>
    </tbody>
  </table>
</template>
