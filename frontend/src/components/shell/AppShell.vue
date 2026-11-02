<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, START_LOCATION, useRoute, useRouter } from 'vue-router'
import type { ChatSummary, LLMInfo } from '../../api/types'
import Icon, { type IconName } from '../Icon.vue'
import MockBanner from '../MockBanner.vue'
import Logo from './Logo.vue'
import LanguageSwitch from './LanguageSwitch.vue'
import { t, tk } from '../../i18n'
import { trapTab } from '../../lib/dialog'

// The app frame: a sidebar on wide screens (an icon rail below 1280px, a
// drawer below 1024px), a tab bar on phones. It shows which model answers
// and whether the senders (workers) are alive.
const props = defineProps<{
  llm: LLMInfo | null
  workers: { alive: number; total: number } | null
  connected: boolean
  chats: ChatSummary[]
}>()

const route = useRoute()
const drawer = ref(false)
const main = ref<HTMLElement | null>(null)
const sidebar = ref<HTMLElement | null>(null)
const menuButton = ref<HTMLButtonElement | null>(null)
const closeButton = ref<HTMLButtonElement | null>(null)

const focusMain = () => main.value?.focus()

// Below 1024px the menu is a drawer over the page: while it is open the page
// behind is inert, focus starts on its close button, Tab stays inside and
// Esc closes it; closing by hand gives focus back to the menu button.
async function openDrawer() {
  drawer.value = true
  await nextTick()
  closeButton.value?.focus()
}
async function closeDrawer(back = true) {
  if (!drawer.value) return
  drawer.value = false
  if (!back) return
  await nextTick()
  menuButton.value?.focus()
}
function onSidebarKey(e: KeyboardEvent) {
  if (!drawer.value) return
  if (e.key === 'Escape') {
    e.preventDefault()
    void closeDrawer()
  } else trapTab(e, sidebar.value)
}

watch(
  () => route.fullPath,
  () => void closeDrawer(false),
)
// Going to another page in the app moves focus to its content, as a page
// load would; a filter or another query change keeps focus where it is, and
// the first page the app opens leaves focus at the top (skip link first).
const stop = useRouter().afterEach((to, from) => {
  if (from === START_LOCATION || to.path === from.path) return
  void nextTick(() => main.value?.focus({ preventScroll: true }))
})
onBeforeUnmount(stop)

const nav: { area: string; to: string; label: string; icon: IconName }[] = [
  { area: 'overview', to: '/', label: tk('Genel bakış'), icon: 'overview' },
  { area: 'campaigns', to: '/campaigns', label: tk('Kampanyalar'), icon: 'campaigns' },
  { area: 'workspace', to: '/assistant', label: tk('Asistan'), icon: 'assistant' },
]
const area = computed(() => String(route.meta.area ?? ''))
const model = computed(() => {
  if (!props.llm) return ''
  return props.llm.provider === 'mock' ? `MOCK · ${props.llm.model}` : `Claude · ${props.llm.model}`
})
</script>

<template>
  <div class="shell" :class="{ 'drawer-open': drawer, fill: route.name === 'workspace' }">
    <a href="#icerik" class="skip-link" @click.prevent="focusMain">{{ t('İçeriğe geç') }}</a>
    <aside ref="sidebar" class="sidebar" :aria-label="t('Ana menü')" @keydown="onSidebarKey">
      <div class="brand">
        <Logo />
        <span class="brand-name">Smart Campaign Composer</span>
        <button ref="closeButton" type="button" class="icon-button drawer-close" :aria-label="t('Menüyü kapat')" @click="closeDrawer()">
          <Icon name="close" />
        </button>
      </div>
      <RouterLink :to="{ name: 'campaign-new' }" class="button primary new-campaign">
        <Icon name="plus" :size="16" /><span class="label">{{ t('Yeni kampanya') }}</span>
      </RouterLink>
      <nav class="side-nav">
        <RouterLink v-for="n in nav" :key="n.area" custom :to="n.to" v-slot="{ href, navigate }">
          <a :href="href" class="nav-item" :aria-current="area === n.area ? 'page' : undefined" :title="t(n.label)" @click="navigate">
            <Icon :name="n.icon" /><span class="label">{{ t(n.label) }}</span>
          </a>
        </RouterLink>
      </nav>
      <section v-if="chats.length" class="recent" :aria-label="t('Son sohbetler')">
        <h2>{{ t('Sohbetler') }}</h2>
        <RouterLink v-for="c in chats" :key="c.id" :to="{ name: 'workspace', params: { chatId: c.id } }" class="recent-chat" :title="c.title">
          {{ c.title || t('Yeni sohbet') }}
        </RouterLink>
      </section>
      <footer class="system">
        <LanguageSwitch />
        <span v-if="model" class="pill" :class="{ mock: llm?.provider === 'mock' }" :title="llm?.reason">{{ model }}</span>
        <span v-if="workers && workers.alive > 0" class="status-line">
          <span class="dot good" aria-hidden="true" />{{ t('Gönderim servisi çalışıyor') }}
        </span>
        <span v-else-if="workers" class="status-line status-warning">
          <Icon name="warning" :size="14" />{{ t('Gönderim servisi yanıt vermiyor') }}
        </span>
      </footer>
    </aside>
    <div class="scrim" aria-hidden="true" @click="closeDrawer()" />

    <div class="main-column" :inert="drawer || undefined">
      <MockBanner :llm="llm" />
      <header class="topbar">
        <button ref="menuButton" type="button" class="icon-button menu-button" :aria-label="t('Menüyü aç')" :aria-expanded="drawer" @click="openDrawer">
          <Icon name="menu" />
        </button>
        <Logo />
        <span class="topbar-title">Smart Campaign Composer</span>
        <LanguageSwitch />
        <span v-if="model" class="pill" :class="{ mock: llm?.provider === 'mock' }">{{ model }}</span>
      </header>
      <main id="icerik" ref="main" class="content" tabindex="-1">
        <slot />
      </main>
    </div>

    <nav class="tabbar" :aria-label="t('Ana menü')" :inert="drawer || undefined">
      <RouterLink v-for="n in nav" :key="n.area" custom :to="n.to" v-slot="{ href, navigate }">
        <a :href="href" class="tab-item" :aria-current="area === n.area ? 'page' : undefined" @click="navigate">
          <Icon :name="n.icon" :size="20" /><span>{{ t(n.label) }}</span>
        </a>
      </RouterLink>
    </nav>
  </div>
</template>
