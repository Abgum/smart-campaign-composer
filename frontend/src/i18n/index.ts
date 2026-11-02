import { ref, watch } from 'vue'
import { en } from './en'

// The UI speaks Turkish and English. Turkish is the source language: the
// Turkish text itself is the key, so it needs no catalog, and English comes
// from en.ts. A text with no English entry shows in Turkish (a test checks
// that every text in the source has one). The choice is remembered in the
// browser and sets <html lang>, so screen readers pronounce the page right;
// the API is asked to answer (errors included) in the same language.

export type Locale = 'tr' | 'en'
export type Params = Record<string, string | number>
export type Message = string | ((p: Params) => string)

export const locales: { value: Locale; label: string; name: string }[] = [
  { value: 'tr', label: 'TR', name: 'Türkçe' },
  { value: 'en', label: 'EN', name: 'English' },
]

const storageKey = 'scc.locale'

function remembered(): Locale | null {
  try {
    const v = globalThis.localStorage?.getItem(storageKey)
    return v === 'tr' || v === 'en' ? v : null
  } catch {
    return null
  }
}

export const locale = ref<Locale>(remembered() ?? 'tr')

export function setLocale(l: Locale) {
  locale.value = l
  try {
    globalThis.localStorage?.setItem(storageKey, l)
  } catch {
    // a blocked storage only means the choice is not remembered
  }
}

watch(
  locale,
  (l) => {
    if (typeof document !== 'undefined') document.documentElement.lang = l
  },
  { immediate: true, flush: 'sync' },
)

// t gives the Turkish source text in the current language, with {name}
// placeholders filled from params.
export function t(source: string, params: Params = {}): string {
  const msg: Message = locale.value === 'en' ? (en[source] ?? source) : source
  const text = typeof msg === 'function' ? msg(params) : msg
  return text.replace(/\{(\w+)\}/g, (m, k: string) => (k in params ? String(params[k]) : m))
}

// tk marks a Turkish text kept in data (a label in a list) to be shown later
// with t(); it returns the text as it is. The catalog check finds these too.
export const tk = (source: string): string => source

// intlLocale is the BCP 47 tag for Intl formatting in the current language.
export const intlLocale = () => (locale.value === 'en' ? 'en-US' : 'tr-TR')

const numbers = { tr: new Intl.NumberFormat('tr-TR'), en: new Intl.NumberFormat('en-US') }

// formatNumber writes a count the current language's way: 1.240 or 1,240.
export function formatNumber(n: number): string {
  return numbers[locale.value].format(n)
}

// langHeader asks the API to answer in the current language.
export function langHeader(): Record<string, string> {
  return { 'Accept-Language': locale.value }
}
