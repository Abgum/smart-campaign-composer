import { t, tk } from '../i18n'

// The languages campaign content can be written in (Go's language.Supported).
export const contentLanguages = ['tr', 'en', 'fr']

const languageNames: Record<string, string> = {
  tr: tk('Türkçe'), en: tk('İngilizce'), fr: tk('Fransızca'), de: tk('Almanca'), ar: tk('Arapça'),
  es: tk('İspanyolca'), it: tk('İtalyanca'), ru: tk('Rusça'), pt: tk('Portekizce'),
}

// The countries of the place catalog.
const countryNames: Record<string, string> = {
  TR: tk('Türkiye'), FR: tk('Fransa'), US: tk('ABD'), GB: tk('Birleşik Krallık'), CA: tk('Kanada'), DE: tk('Almanya'),
}

export function languageName(code: string): string {
  if (!code) return t('Dil belirtilmemiş')
  const name = languageNames[code]
  return name ? t(name) : code
}

export function countryName(code: string): string {
  const name = countryNames[code]
  return name ? t(name) : code
}

// groupLabel names a group of recipients who wait for a language decision:
// an unsupported preference, a country without a language for those with no
// preference, or a language the draft has no variant for.
export function groupLabel(group: string): string {
  const at = group.indexOf(':')
  const kind = group.slice(0, at)
  const value = group.slice(at + 1)
  switch (kind) {
    case 'preference':
      return t('Tercihi {language}', { language: languageName(value) })
    case 'country':
      return t('{country}, tercihi kayıtlı değil', { country: countryName(value) })
    case 'variant':
      return t('{language} okuyanlar, bu dilde varyant yok', { language: languageName(value) })
  }
  return group
}

// orderLanguages lists the supported languages first, in their order.
export function orderLanguages(codes: string[]): string[] {
  const rank = (c: string) => {
    const i = contentLanguages.indexOf(c)
    return i < 0 ? contentLanguages.length : i
  }
  return [...codes].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
}
