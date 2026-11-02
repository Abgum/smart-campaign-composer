import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import LanguagePreview from '../assistant/LanguagePreview.vue'
import CampaignEditor from './CampaignEditor.vue'
import ApproveModal from '../ApproveModal.vue'
import { ApiError } from '../../api/http'
import type {
  ApprovalPreview,
  ApproveResult,
  CampaignDetail,
  LanguageSummary,
  Localization,
  ManualDraftInput,
  RuleCatalog,
  Version,
} from '../../api/types'

const loc: Localization = {
  mode: 'per_recipient',
  variants: [
    { language: 'tr', sms: { text: 'Merhaba {{first_name}}' }, email: { subject: 'Sepetin', body: 'Merhaba' } },
    { language: 'en', sms: { text: 'Hi {{first_name}}' }, email: { subject: 'Your cart', body: 'Hi' } },
    { language: 'fr', sms: { text: 'Bonjour {{first_name}}' }, email: { subject: 'Votre panier', body: 'Bonjour' } },
  ],
  country_languages: [{ country: 'TR', language: 'tr' }, { country: 'FR', language: 'fr' }],
  decisions: [{ group: 'country:CA', action: 'exclude' }],
}
const summary: LanguageSummary = {
  mode: 'per_recipient',
  languages: { tr: { total: 300, sms: 250, email: 280 }, en: { total: 3, sms: 3, email: 3 }, fr: { total: 2, sms: 2, email: 2 } },
  unresolved: [{ group: 'preference:de', count: 4 }, { group: 'country:DE', count: 2 }],
  excluded: [{ group: 'country:CA', count: 1 }],
  countries: { TR: 20, FR: 2, CA: 1 }, // by country, who the mapping decides and can get a message
}
const catalog: RuleCatalog = {
  fields: [{ name: 'age', type: 'int', operators: ['gte'], min: 0, max: 120, description: 'Age' }],
  events: [{ name: 'cart_add', description: 'Cart', not_followed_by: ['purchase'] }], languages: ['tr', 'en', 'fr'],
  places: [
    { level: 'country', code: 'TR', name: 'Türkiye' }, { level: 'country', code: 'FR', name: 'Fransa' },
    { level: 'country', code: 'CA', name: 'Kanada' }, { level: 'country', code: 'DE', name: 'Almanya' },
  ],
  max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20,
}
const version = (id: number, localization: Localization | null = loc): Version => ({
  id, version: id - 40, source: 'llm', llm_provider: 'mock', name: 'Diller',
  segment_rule: { match: 'all', conditions: [{ kind: 'event', event_type: 'cart_add', within_days: 30, not_followed_by: 'purchase' }] },
  sms: null, email: null, change_summary: '', created_at: '2026-10-30T10:00:00Z', localization, language_summary: summary,
})

// What every count of the language section is, said next to the counts.
const scope = 'Sayılar taslak anına ait, onayda yeniden hesaplanır: kampanyanın açık kanallarından en az birinden mesaj alabilecek alıcılar; iki kanala da uygun olan bir kez sayılır.'
const mappingNote = 'Satırdaki sayı, o ülkede dili bu eşlemeyle belirlenen ve mesaj alabilecek alıcılardır; desteklenen dil tercihi olanlar sayılmaz.'

describe('LanguagePreview', () => {
  it('shows each variant with its recipients, and who waits for a decision', async () => {
    const w = mount(LanguagePreview, { props: { localization: loc, summary, sms: null, email: null } })
    const tabs = w.findAll('[role="tab"]')
    expect(tabs.map((tab) => tab.text())).toEqual(['Türkçe · 300', 'İngilizce · 3', 'Fransızca · 2'])
    expect(w.get('.language-scope').text()).toBe(scope)
    expect(w.get('.sms-preview').text()).toContain('Merhaba')
    await tabs[1].trigger('click')
    expect(tabs[1].attributes('aria-selected')).toBe('true')
    expect(w.get('.sms-preview').text()).toContain('Hi')
    const waiting = w.get('.language-waiting').text()
    expect(waiting).toContain('6 alıcı dil kararı bekliyor')
    expect(waiting).toContain('Tercihi Almanca: 4')
    expect(waiting).not.toContain('(de)')
    expect(waiting).toContain('Almanya, tercihi kayıtlı değil: 2')
  })

  it('keeps the country mapping, with the recipients each line decides, under advanced settings', () => {
    const w = mount(LanguagePreview, { props: { localization: loc, summary, sms: null, email: null } })
    const advanced = w.get('details.language-advanced')
    expect(advanced.attributes('open')).toBeUndefined()
    expect(advanced.get('summary').text()).toBe('Gelişmiş dil ayarları')
    expect(advanced.findAll('li').map((li) => li.text())).toEqual(['Türkiye → Türkçe · 20 alıcı', 'Fransa → Fransızca · 2 alıcı'])
    // The count says who it leaves out: no message possible, or a supported preference.
    expect(advanced.get('.mapping-note').text()).toBe(mappingNote)
  })

  it('leaves the counts out for a version saved before they were counted', () => {
    const { countries: _, ...older } = summary
    const w = mount(LanguagePreview, { props: { localization: loc, summary: older, sms: null, email: null } })
    expect(w.get('details.language-advanced').findAll('li').map((li) => li.text())).toEqual(['Türkiye → Türkçe', 'Fransa → Fransızca'])
    expect(w.find('.mapping-note').exists()).toBe(false)
  })

  it('names the one language of a single content', () => {
    const w = mount(LanguagePreview, {
      props: { localization: { mode: 'single', language: 'en' }, summary: null, sms: { text: 'Hi' }, email: null },
    })
    expect(w.text()).toContain('Herkese aynı metin · İngilizce')
    expect(w.find('[role="tab"]').exists()).toBe(false)
    expect(w.find('.language-advanced').exists()).toBe(false)
    expect(w.find('.language-scope').exists()).toBe(false)
  })
})

describe('CampaignEditor with languages', () => {
  it('edits each variant on its own and keeps the language setup', async () => {
    const w = mount(CampaignEditor, { props: { initial: version(42), catalog } })
    const tabs = w.findAll('.variant-tabs [role="tab"]')
    expect(tabs.map((tab) => tab.text())).toEqual(['Türkçe', 'İngilizce', 'Fransızca'])
    await tabs[1].trigger('click')
    const sms = w.get('[data-test="sms-text"]')
    expect((sms.element as HTMLTextAreaElement).value).toBe('Hi {{first_name}}')
    await sms.setValue('Hello {{first_name}}')
    await w.get('form.campaign-editor').trigger('submit')
    const draft = w.emitted('save')![0][0] as ManualDraftInput
    expect(draft.sms).toBeNull()
    expect(draft.email).toBeNull()
    expect(draft.localization?.variants?.map((v) => v.sms?.text)).toEqual(['Merhaba {{first_name}}', 'Hello {{first_name}}', 'Bonjour {{first_name}}'])
    expect(draft.localization?.decisions).toEqual(loc.decisions)
    expect(draft.localization?.country_languages).toEqual(loc.country_languages)
  })

  it('sums up the languages first and keeps the country mapping under advanced settings', () => {
    const w = mount(CampaignEditor, { props: { initial: version(42), catalog } })
    const section = w.get('.editor-languages')
    expect(section.get('.language-distribution').text()).toBe('Dil dağılımı: Türkçe 300 · İngilizce 3 · Fransızca 2')
    expect(section.get('.language-scope').text()).toContain(scope)
    expect(section.text()).toContain('6 alıcı dil kararı bekliyor')
    const advanced = section.get('details.language-advanced')
    expect(advanced.attributes('open')).toBeUndefined()
    expect(advanced.get('summary').text()).toBe('Gelişmiş dil ayarları')
    expect(advanced.findAll('.mapping-row')).toHaveLength(2)
    // Who each line decides: recipients there without a supported preference
    // who can get a message on one of the campaign's channels.
    expect(advanced.findAll('.mapping-row .mapping-count').map((c) => c.text())).toEqual(['20 alıcı', '2 alıcı'])
    expect(advanced.get('.mapping-note').text()).toBe(mappingNote)
  })

  it('explains that unsaved channel changes do not recalculate the displayed counts', async () => {
    const w = mount(CampaignEditor, { props: { initial: version(42), catalog } })
    await w.get('[data-test="sms-on"]').setValue(false)
    expect(w.get('.language-distribution').text()).toBe('Dil dağılımı: Türkçe 300 · İngilizce 3 · Fransızca 2')
    expect(w.get('.language-scope').text()).toContain('Sayılar son kaydedilen sürüme aittir; bu ekrandaki değişiklikler kaydedilene kadar güncellenmez.')
  })

  it('edits the country mapping by hand, keeping the decisions', async () => {
    const w = mount(CampaignEditor, { props: { initial: version(42), catalog } })
    const rows = () => w.findAll('.mapping-row')
    expect(rows()).toHaveLength(2)
    await w.get('[data-test="mapping-language-1"]').setValue('en') // France → English
    await w.get('[data-test="mapping-add"]').trigger('click') // the first free country: Canada
    expect((w.get('[data-test="mapping-country-2"]').element as HTMLSelectElement).value).toBe('CA')
    expect(w.get('[data-test="mapping-country-2"]').findAll('option').map((o) => o.text())).toEqual(['Kanada', 'Almanya'])
    await w.get('[data-test="mapping-language-2"]').setValue('fr')
    expect(w.get('.mapping-row:nth-of-type(3) .mapping-count').text()).toBe('1 alıcı')
    await w.get('[data-test="mapping-remove-0"]').trigger('click') // drop Türkiye
    await w.get('form.campaign-editor').trigger('submit')
    const draft = w.emitted('save')![0][0] as ManualDraftInput
    expect(draft.localization?.country_languages).toEqual([{ country: 'FR', language: 'en' }, { country: 'CA', language: 'fr' }])
    expect(draft.localization?.decisions).toEqual(loc.decisions)
    expect(draft.localization?.variants).toEqual(loc.variants)
  })

  it('states the language of one content', async () => {
    const w = mount(CampaignEditor, {
      props: { initial: { ...version(42, { mode: 'single', language: 'tr' }), sms: { text: 'Merhaba' } }, catalog },
    })
    expect((w.get('[data-test="content-language"]').element as HTMLSelectElement).value).toBe('tr')
    // One content for everyone has no country mapping to show.
    expect(w.find('.editor-languages').exists()).toBe(false)
    expect(w.find('.language-advanced').exists()).toBe(false)
    await w.get('[data-test="content-language"]').setValue('en')
    await w.get('[data-test="sms-text"]').setValue('Hello')
    await w.get('form.campaign-editor').trigger('submit')
    expect(w.emitted('save')![0][0]).toMatchObject({ sms: { text: 'Hello' }, localization: { mode: 'single', language: 'en' } })
  })
})

describe('ApproveModal with languages', () => {
  const campaign = (id = 42): CampaignDetail => ({
    id: 'c-1', chat_id: 'chat', ordinal: 1, name: 'Diller', status: 'draft', current_version: version(id),
    approved_version_id: null, approval: null, versions: [], updated_at: '2026-10-30T10:00:00Z',
  })
  const counts = (selected: number, consent = 0, duplicate = 0) => ({ selected, excluded_consent: consent, excluded_duplicate: duplicate })
  const preview = (over: Partial<ApprovalPreview> = {}): ApprovalPreview => ({
    version_id: 42, mode: 'per_recipient',
    channels: { sms: counts(255, 50, 1), email: counts(285) },
    languages: { tr: { sms: counts(250, 50, 1), email: counts(280) }, en: { sms: counts(3), email: counts(3) }, fr: { sms: counts(2), email: counts(2) } },
    unresolved: [], excluded_language: [{ group: 'country:CA', count: 1 }],
    country_languages: loc.country_languages ?? [], decisions: loc.decisions ?? [],
    samples: [
      { language: 'tr', channel: 'sms', body: 'Merhaba Ayşe' },
      { language: 'en', channel: 'email', subject: 'Your cart', body: 'Hi Emily' },
    ],
    issues: [], ...over,
  })
  const approved = (tr: number): ApproveResult => ({
    campaign_id: 'c-1', version_id: 42, status: 'approved', already_approved: false,
    channels: { sms: counts(tr + 5), email: counts(285) },
    languages: { tr: { sms: counts(tr), email: counts(280) }, en: { sms: counts(3), email: counts(3) }, fr: { sms: counts(2), email: counts(2) } },
    excluded_language: [{ group: 'country:CA', count: 1 }],
  })

  it('shows recipients per language and channel, who is left out and every message before approving', async () => {
    const w = mount(ApproveModal, {
      props: { campaign: campaign(), approve: vi.fn(), preview: vi.fn().mockResolvedValue(preview()), saveDecisions: vi.fn() },
    })
    await flushPromises()
    const row = (name: string) => w.findAll('.language-table tbody tr').find((r) => r.text().startsWith(name))?.findAll('td').map((td) => td.text())
    expect(row('Türkçe')).toEqual(['250', '280'])
    expect(row('İngilizce')).toEqual(['3', '3'])
    expect(w.get('.language-excluded').text()).toContain('Kanada, tercihi kayıtlı değil: 1')
    expect(w.text()).toContain('1 kişi bu içeriği zaten aldı / alacak')
    expect(w.get('.language-mapping').text()).toContain('Türkiye → Türkçe')
    expect(w.get('.language-mapping').text()).not.toContain('TR →')
    const samples = w.get('.language-samples').text()
    expect(samples).toContain('Merhaba Ayşe')
    expect(samples).toContain('Your cart')
    expect(w.get('button.confirm').attributes('disabled')).toBeUndefined()
  })

  it('asks for a decision on waiting recipients before sending can start', async () => {
    const saveDecisions = vi.fn().mockResolvedValue(undefined)
    const previewFn = vi.fn().mockResolvedValue(preview({ unresolved: [{ group: 'preference:de', count: 4 }] }))
    const w = mount(ApproveModal, { props: { campaign: campaign(), approve: vi.fn(), preview: previewFn, saveDecisions } })
    await flushPromises()
    expect(w.get('button.confirm').attributes('disabled')).toBeDefined()
    const select = w.get('select[data-group="preference:de"]')
    expect(select.findAll('option').map((o) => o.text())).toEqual(['Seçin…', 'Türkçe', 'İngilizce', 'Fransızca', 'Gönderme'])
    await select.setValue('en')
    await w.get('button.save-decisions').trigger('click')
    await flushPromises()
    expect(saveDecisions).toHaveBeenCalledWith(42, {
      ...loc,
      decisions: [{ group: 'country:CA', action: 'exclude' }, { group: 'preference:de', action: 'assign', language: 'en' }],
    })
    // The parent loads the new version; the preview follows it.
    await w.setProps({ campaign: campaign(43) })
    await flushPromises()
    expect(previewFn).toHaveBeenLastCalledWith(43)
  })

  it('stops when new recipients wait at approval, then shows what changed since the preview', async () => {
    const approve = vi.fn()
      .mockRejectedValueOnce(new ApiError(409, 'language_decision_required', 'gönderim başlamadı: bazı alıcılar için dil kararı gerekiyor',
        { groups: [{ group: 'preference:ar', count: 1 }] }))
      .mockResolvedValueOnce(approved(251))
    const previewFn = vi.fn()
      .mockResolvedValueOnce(preview())
      .mockResolvedValueOnce(preview({ unresolved: [{ group: 'preference:ar', count: 1 }] }))
      .mockResolvedValue(preview())
    const w = mount(ApproveModal, { props: { campaign: campaign(), approve, preview: previewFn, saveDecisions: vi.fn() } })
    await flushPromises()
    await w.get('button.confirm').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('gönderim başlamadı')
    expect(w.find('select[data-group="preference:ar"]').exists()).toBe(true)
    expect(w.emitted('approved')).toBeUndefined()

    await w.setProps({ campaign: campaign(44) })
    await flushPromises()
    await w.get('button.confirm').trigger('click')
    await flushPromises()
    expect(w.emitted('approved')).toHaveLength(1)
    expect(w.get('.language-diff').text()).toContain('Türkçe · SMS: 250 → 251')
  })
})
