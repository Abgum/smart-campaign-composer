<script setup lang="ts">
import { t, tk } from '../i18n'
import { computed, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { analyticsQuery, filtersQuery, parseFilters, type Filters, type Range } from '../lib/filters'
import { foldCities } from '../lib/campaigns'
import { dayName, formatCount } from '../lib/format'
import { useAnalytics, useCampaigns, useMedia, useRequests } from '../lib/data'
import { useLiveStore } from '../stores/live'
import { reducedMotion } from '../lib/motion'
import Icon from '../components/Icon.vue'
import StatTile from '../components/charts/StatTile.vue'
import StackedColumns from '../components/charts/StackedColumns.vue'
import CityBars from '../components/charts/CityBars.vue'
import FilterCard from '../components/overview/FilterCard.vue'
import CampaignList from '../components/overview/CampaignList.vue'
import RecentRequests from '../components/overview/RecentRequests.vue'
import type { OutcomeFilter } from '../components/overview/CampaignTable.vue'
import EmptyState from '../components/overview/EmptyState.vue'

// The overview (plan §19): a title, the filter card, four KPIs, sends by
// day and by city, then marketers' latest requests and the campaigns. All
// numbers are for the slice in the URL and come from stored data; they are
// refetched when the live stream says something changed.
const route = useRoute()
const router = useRouter()
const live = useLiveStore()
const phone = useMedia('(max-width: 767px)')

const filters = computed(() => parseFilters(route.query))
const version = () => live.version
const analytics = useAnalytics(() => analyticsQuery(filters.value, 10), version)
const campaigns = useCampaigns(() => 'limit=100', version)
const requests = useRequests(6, version)

function update(f: Filters) {
  void router.replace({ query: filtersQuery(f) })
}
const setRange = (range: Range) => update({ ...filters.value, range })

const data = computed(() => analytics.data.value)
const kpis = computed(() => data.value?.kpis)
const approved = computed(() => campaigns.list().filter((c) => c.status !== 'draft'))
const listed = computed(() =>
  campaigns
    .list()
    .filter((c) => filters.value.campaign === null || c.id === filters.value.campaign)
    .slice(0, 6),
)
const campaignOptions = computed(() => approved.value.map((c) => ({ id: c.id, label: `${t('Kampanya {n}', { n: c.ordinal })} · ${c.name}` })))

const channels = computed(() => (filters.value.channel ? [filters.value.channel] : (['sms', 'email'] as const).slice()))
const nothingYet = computed(() => !!data.value && campaigns.data.value !== null && approved.value.length === 0)
const rangeEmpty = computed(() => !!kpis.value && kpis.value.sent.total === 0)
const seriesTitle = computed(() => {
  const w = data.value?.window
  if (!w || w.bucket === 'day') return t('Günlük gönderim')
  return w.range === 'day' ? t('{day} saatlik gönderim', { day: dayName(w.from) }) : t('Bugün saatlik gönderim')
})
// onlyDay is the one day with sends when the daily chart has just one; it
// can be opened by hour. The window's last day is today.
const onlyDay = computed(() => {
  const d = data.value
  if (!d || d.window.bucket !== 'day' || d.series.length < 2) return null
  const active = d.series.filter((p) => p.sms + p.email > 0)
  return active.length === 1 ? { at: active[0].at, today: active[0].at === d.window.to } : null
})
function byHour(day: { at: string; today: boolean }) {
  if (day.today) setRange('today')
  else update({ ...filters.value, range: 'day', day: day.at })
}
const cities = computed(() => (data.value ? (phone.value ? foldCities(data.value.cities, 5) : data.value.cities) : null))
const stalled = computed(() => live.workers?.alive === 0 && (kpis.value?.queued.total ?? 0) > 0)

const outcome = ref<OutcomeFilter>(null)
const list = ref<HTMLElement | null>(null)
function pick(o: Exclude<OutcomeFilter, null>) {
  outcome.value = outcome.value === o ? null : o
  list.value?.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'start' })
}
const outcomeLabel: Record<Exclude<OutcomeFilter, null>, string> = {
  sent: tk('Yalnız gönderilen mesajı olanlar'),
  queued: tk('Yalnız kuyrukta bekleyen mesajı olanlar'),
  failed: tk('Yalnız başarısız mesajı olanlar'),
  unknown: tk('Yalnız belirsiz mesajı olanlar'),
}
</script>

<template>
  <section class="page overview">
    <header class="hero">
      <div class="hero-text">
        <h1 class="hero-title">{{ t('Genel bakış') }}</h1>
        <p class="hero-sub">{{ t('Kampanyalarınızın gönderim durumu: kaç mesaj gitti, hangi günlerde ve hangi şehirlere.') }}</p>
      </div>
      <div class="hero-actions">
        <span class="live-pill" :class="{ on: live.connected }"><span class="live-dot" aria-hidden="true" />{{ live.connected ? t('Canlı') : t('Bağlanıyor') }}</span>
        <RouterLink :to="{ name: 'campaign-new' }" class="button primary hero-new"><Icon name="plus" :size="16" />{{ t('Yeni kampanya') }}</RouterLink>
      </div>
    </header>

    <FilterCard :filters="filters" :campaigns="campaignOptions" @update="update" />

    <div v-if="analytics.error.value" class="alert-strip error" role="alert">
      <Icon name="warning" :size="16" />{{ t('Veriler şu an alınamadı.') }}
      <button type="button" class="link" @click="analytics.reload()">{{ t('Yeniden dene') }}</button>
    </div>
    <div v-if="stalled" class="alert-strip" role="alert">
      <Icon name="warning" :size="16" />{{ t('Gönderim şu an ilerlemiyor: kuyruktaki mesajlar bekliyor. Sistem yöneticinize haber verin.') }}
    </div>

    <div v-if="kpis" class="kpi-grid" :class="{ refreshing: analytics.loading.value }">
      <StatTile
        :label="t('Gönderildi')"
        icon="sent"
        tone="good"
        :value="kpis.sent.total"
        :split="kpis.sent"
        :info="t('Seçili aralıkta sağlayıcının kabul ettiği mesajlar.')"
        :selected="outcome === 'sent'"
        @select="pick('sent')"
      />
      <StatTile
        :label="t('Kuyrukta')"
        icon="queued"
        tone="accent"
        :value="kpis.queued.total"
        :split="kpis.queued"
        :note="kpis.queued.scheduled ? t('{n} mesaj zamanlanmış', { n: formatCount(kpis.queued.scheduled) }) : undefined"
        :info="t('Şu an gönderilmeyi bekleyen mesajlar (aralıktan bağımsız). Gönderim zamanı gelmemiş zamanlanmış mesajlar dahil değil.')"
        :selected="outcome === 'queued'"
        @select="pick('queued')"
      />
      <StatTile
        :label="t('Başarısız')"
        icon="failed"
        tone="critical"
        :value="kpis.failed.total"
        :split="kpis.failed"
        :info="t('Seçili aralıkta kesin gönderilemeyen mesajlar.')"
        :selected="outcome === 'failed'"
        @select="pick('failed')"
      />
      <StatTile
        :label="t('Belirsiz')"
        icon="unknown"
        tone="warning"
        :value="kpis.unknown.total"
        :split="kpis.unknown"
        :info="t('Sağlayıcı kabul etmiş olabilir ama yanıt alınamadı; aynı kişiye tekrar gönderilmez.')"
        :selected="outcome === 'unknown'"
        @select="pick('unknown')"
      />
    </div>
    <div v-else-if="!analytics.error.value" class="kpi-grid" aria-hidden="true">
      <span v-for="i in 4" :key="i" class="skeleton-block" />
    </div>

    <EmptyState
      v-if="nothingYet"
      icon="chart"
      :title="t('Henüz gönderim yok')"
      :text="t('Bir kampanyayı onayladığınızda gönderimler, günlere ve şehirlere göre dağılımı burada görünür.')"
      :action="{ label: t('Yeni kampanya'), to: { name: 'campaign-new' } }"
    />

    <template v-else-if="data">
      <div class="chart-grid" :class="{ refreshing: analytics.loading.value }">
        <section class="card chart-card">
          <header class="card-head">
            <h2 class="card-title">{{ seriesTitle }}</h2>
            <p class="card-sub">{{ t('Gönderilen mesajlar, {tz} saatine göre', { tz: data.window.tz === 'Europe/Istanbul' ? t('İstanbul') : data.window.tz }) }}</p>
          </header>
          <div v-if="rangeEmpty" class="in-frame-empty">
            <p>{{ t('Bu aralıkta gönderim yok.') }}</p>
            <div class="range-shortcuts">
              <button v-if="filters.range !== '30d'" type="button" @click="setRange('30d')">{{ t('Son 30 gün') }}</button>
              <button v-if="filters.range !== 'all'" type="button" @click="setRange('all')">{{ t('Tümü') }}</button>
            </div>
          </div>
          <template v-else>
            <StackedColumns :points="data.series" :bucket="data.window.bucket" :channels="channels" />
            <p v-if="onlyDay" class="chart-note">
              {{ t('Bu aralıkta yalnız 1 günde gönderim var ({day}).', { day: dayName(onlyDay.at) }) }}
              <button type="button" class="link" @click="byHour(onlyDay)">{{ onlyDay.today ? t('Bugünü saatlik gör') : t('O günü saatlik gör') }}</button>
            </p>
          </template>
        </section>
        <section class="card chart-card">
          <header class="card-head">
            <h2 class="card-title">{{ t('En çok gönderim yapılan şehirler') }}</h2>
            <p class="card-sub">{{ t('Alıcının şehrine göre, ilk {n}', { n: phone ? 5 : 10 }) }}</p>
          </header>
          <CityBars v-if="cities && (cities.top.length || cities.unknown.total)" :cities="cities" :channels="channels" />
          <p v-else class="in-frame-empty">{{ t('Şehir dağılımı ilk gönderimlerden sonra oluşur.') }}</p>
        </section>
      </div>

      <div class="bottom-grid">
        <section class="card list-card">
          <header class="card-head row">
            <div>
              <h2 class="card-title">{{ t('Son kampanya istekleri') }}</h2>
              <p class="card-sub">{{ t('Pazarlama ekibinin asistana yazdıkları') }}</p>
            </div>
            <RouterLink :to="{ name: 'workspace' }" class="head-link">{{ t('Asistana git') }}</RouterLink>
          </header>
          <RecentRequests :requests="requests.list()" />
        </section>
        <section ref="list" class="card list-card">
          <header class="card-head row">
            <div>
              <h2 class="card-title">{{ t('Kampanyalar') }}</h2>
              <p class="card-sub">
                <template v-if="outcome">
                  {{ t(outcomeLabel[outcome]) }} ·
                  <button type="button" class="link" @click="outcome = null">{{ t('Tümünü göster') }}</button>
                </template>
                <template v-else>{{ t('En son oluşturulanlar') }}</template>
              </p>
            </div>
            <RouterLink :to="{ name: 'campaigns' }" class="head-link">{{ t('Tümünü gör') }}</RouterLink>
          </header>
          <CampaignList :campaigns="outcome ? approved : listed" :status="outcome" :channel="filters.channel" />
        </section>
      </div>
    </template>
  </section>
</template>
