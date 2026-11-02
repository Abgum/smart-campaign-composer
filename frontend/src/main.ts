import { createApp, watch } from 'vue'
import { createPinia } from 'pinia'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import { legacyRedirect, routes } from './router'
import { locale, t } from './i18n'
import './styles.css'

// Links of the first UI (#/chat/<id>, #/dashboard) open the new pages.
const legacy = legacyRedirect(location.hash)
if (legacy) history.replaceState(null, '', legacy)

const router = createRouter({ history: createWebHistory(), routes })
// The tab names the page, in the current language.
function name(meta: Record<string | symbol, unknown>) {
  document.title = meta.title ? `${t(String(meta.title))} · Smart Campaign Composer` : 'Smart Campaign Composer'
}
router.afterEach((to) => name(to.meta))
watch(locale, () => name(router.currentRoute.value.meta))

createApp(App).use(createPinia()).use(router).mount('#app')
