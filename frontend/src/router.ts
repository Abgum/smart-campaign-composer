import type { RouteRecordRaw } from 'vue-router'
import { tk } from './i18n'

// The app's three areas. meta.area tells the navigation which one is open.
export const routes: RouteRecordRaw[] = [
  { path: '/', name: 'overview', component: () => import('./views/OverviewView.vue'), meta: { area: 'overview', title: tk('Genel bakış') } },
  {
    path: '/campaigns',
    name: 'campaigns',
    component: () => import('./views/CampaignsView.vue'),
    meta: { area: 'campaigns', title: tk('Kampanyalar') },
  },
  { path: '/campaigns/new', name: 'campaign-new', component: () => import('./views/NewCampaignView.vue'), meta: { area: 'campaigns', title: tk('Yeni kampanya') } },
  {
    path: '/campaigns/:id([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})',
    name: 'campaign',
    component: () => import('./views/CampaignDetailView.vue'),
    meta: { area: 'campaigns', title: tk('Kampanya') },
  },
  {
    path: '/assistant/:chatId?',
    name: 'workspace',
    component: () => import('./views/WorkspaceView.vue'),
    meta: { area: 'workspace', title: tk('Asistan') },
  },
  { path: '/:rest(.*)*', redirect: '/' },
]

// legacyRedirect maps the first UI's hash links (#/chat/<id>, #/dashboard)
// to the new pages.
export function legacyRedirect(hash: string): string | null {
  const chat = hash.match(/^#\/chat\/([0-9a-fA-F-]{36})$/)
  if (chat) return `/assistant/${chat[1]}`
  if (hash === '#/dashboard') return '/'
  return null
}
