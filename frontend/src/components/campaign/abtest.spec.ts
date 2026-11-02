import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CampaignEditor from './CampaignEditor.vue'
import AbResults from './AbResults.vue'
import ApproveModal from '../ApproveModal.vue'
import LanguagePreview from '../assistant/LanguagePreview.vue'
import type { ApprovalPreview, ApproveResult, CampaignDetail, ManualDraftInput, RuleCatalog, Version } from '../../api/types'

const catalog: RuleCatalog = {
  fields: [{ name: 'age', type: 'int', operators: ['gte'], min: 0, max: 120, description: 'Age' }],
  events: [], languages: ['tr', 'en'],
  places: [], max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20,
}
const rule = { match: 'all' as const, conditions: [{ kind: 'attribute' as const, field: 'age', operator: 'gte', values: ['18'] }] }
const single: Version = {
  id: 4, version: 2, source: 'llm', llm_provider: 'mock', name: 'Sepet', segment_rule: rule,
  sms: { text: 'Merhaba {{first_name}}' }, email: null, localization: { mode: 'single', language: 'tr' },
  change_summary: '', created_at: '2026-10-28T10:00:00Z',
}
const perRecipient: Version = {
  ...single, sms: null,
  localization: { mode: 'per_recipient', variants: [{ language: 'tr', sms: { text: 'TR-A' } }, { language: 'en', sms: { text: 'EN-A' } }] },
  ab_test: { variants: [{ language: 'tr', sms: { text: 'TR-B' } }, { language: 'en', sms: { text: 'EN-B' } }] },
}
const saved = (w: ReturnType<typeof mount>) => w.emitted('save')?.[0]?.[0] as ManualDraftInput

describe('A/B in the editor', () => {
  it('turns on an A/B test, edits B apart from A and saves the B side', async () => {
    const w = mount(CampaignEditor, { props: { initial: single, catalog } })
    expect(w.find('[data-test="arm-B"]').exists()).toBe(false)
    await w.get('[data-test="ab-on"]').setValue(true)
    await w.get('[data-test="arm-B"]').trigger('click')
    const sms = w.get<HTMLTextAreaElement>('[data-test="sms-text"]')
    expect(sms.element.value).toBe('Merhaba {{first_name}}') // B starts as a copy of A
    await sms.setValue('Son gün {{first_name}}!')
    await w.get('[data-test="arm-A"]').trigger('click')
    expect(w.get<HTMLTextAreaElement>('[data-test="sms-text"]').element.value).toBe('Merhaba {{first_name}}')
    await w.get('form.campaign-editor').trigger('submit')
    expect(saved(w)).toMatchObject({
      sms: { text: 'Merhaba {{first_name}}' },
      ab_test: { variants: [{ language: 'tr', sms: { text: 'Son gün {{first_name}}!' }, email: null }] },
    })
  })

  it('will not save a B that says what A says', async () => {
    const w = mount(CampaignEditor, { props: { initial: single, catalog } })
    await w.get('[data-test="ab-on"]').setValue(true)
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('B, A ile aynı')
  })

  it('keeps a B per language and saves them all; turning the test off removes it', async () => {
    const w = mount(CampaignEditor, { props: { initial: perRecipient, catalog } })
    expect(w.get<HTMLInputElement>('[data-test="ab-on"]').element.checked).toBe(true)
    await w.get('[data-test="arm-B"]').trigger('click')
    const tabs = w.findAll('.variant-tabs button[role="tab"]')
    await tabs[1].trigger('click')
    expect(w.get<HTMLTextAreaElement>('[data-test="sms-text"]').element.value).toBe('EN-B')
    await w.get('[data-test="sms-text"]').setValue('EN-B2')
    await w.get('form.campaign-editor').trigger('submit')
    expect(saved(w).ab_test).toEqual({
      variants: [
        { language: 'tr', sms: { text: 'TR-B' }, email: null },
        { language: 'en', sms: { text: 'EN-B2' }, email: null },
      ],
    })

    const off = mount(CampaignEditor, { props: { initial: perRecipient, catalog } })
    await off.get('[data-test="ab-on"]').setValue(false)
    await off.get('form.campaign-editor').trigger('submit')
    expect(saved(off).ab_test).toBeNull()
  })
})

describe('A/B in the draft preview', () => {
  it('shows A or B of the language picked, and says how recipients split', async () => {
    const w = mount(LanguagePreview, {
      props: { localization: perRecipient.localization, summary: null, sms: null, email: null, abTest: perRecipient.ab_test },
    })
    expect(w.text()).toContain('TR-A')
    await w.get('[data-test="preview-arm-B"]').trigger('click')
    expect(w.text()).toContain('TR-B')
    await w.findAll('.variant-tabs button[role="tab"]')[1].trigger('click')
    expect(w.text()).toContain('EN-B')
    expect(w.text()).toContain('yaklaşık yarısı')
  })

  it('has no A/B switch without a test', () => {
    const w = mount(LanguagePreview, { props: { localization: single.localization, summary: null, sms: single.sms ?? null, email: null } })
    expect(w.find('[data-test="preview-arm-B"]').exists()).toBe(false)
  })
})

describe('A/B in the approval dialog', () => {
  const counts = (selected: number, consent = 0, duplicate = 0) => ({ selected, excluded_consent: consent, excluded_duplicate: duplicate })
  const campaign: CampaignDetail = {
    id: 'c-1', chat_id: 'chat', ordinal: 1, name: 'A/B', status: 'draft', current_version: single,
    approved_version_id: null, approval: null, versions: [], updated_at: '2026-10-30T10:00:00Z',
  }
  const preview: ApprovalPreview = {
    version_id: 4, mode: 'single',
    channels: { sms: counts(9, 0, 3) }, languages: { tr: { sms: counts(9, 0, 3) } },
    variants: { A: { sms: counts(4, 0, 3) }, B: { sms: counts(5) } },
    unresolved: [], excluded_language: [], country_languages: [], decisions: [],
    samples: [
      { language: 'tr', channel: 'sms', variant: 'A', body: 'Merhaba Ayşe' },
      { language: 'tr', channel: 'sms', variant: 'B', body: 'Son gün Ayşe!' },
    ],
    issues: [],
  }

  it('shows each arm with its recipients and who it leaves out, and a message of each arm', async () => {
    const approve = vi.fn().mockResolvedValue({
      campaign_id: 'c-1', version_id: 4, status: 'approved', already_approved: false,
      channels: { sms: counts(9, 0, 3) }, variants: { A: { sms: counts(4, 0, 3) }, B: { sms: counts(5) } },
    } satisfies ApproveResult)
    const w = mount(ApproveModal, { props: { campaign, approve, preview: vi.fn().mockResolvedValue(preview) } })
    await flushPromises()
    const row = (arm: string) => w.findAll('.ab-table tbody tr').find((r) => r.text().startsWith(arm))?.findAll('td').map((td) => td.text())
    expect(row('A')).toEqual(['4', '3'])
    expect(row('B')).toEqual(['5', '0'])
    const samples = w.get('.language-samples').text()
    expect(samples).toContain('Türkçe · SMS · A')
    expect(samples).toContain('Son gün Ayşe!')

    await w.get('button.confirm').trigger('click')
    await flushPromises()
    expect(w.findAll('.ab-table tbody tr').map((r) => r.text())).toEqual(['A43', 'B50'])
  })
})

describe('A/B on the campaign page', () => {
  it('splits the delivery outcomes by arm', async () => {
    const zero = { excluded_consent: 0, excluded_duplicate: 0, pending: 0, in_flight: 0, retry: 0, sent: 0, failed: 0, unknown: 0, skipped_no_consent: 0 }
    const w = mount(AbResults, {
      props: { variants: { A: { sms: { ...zero, sent: 40, unknown: 1 } }, B: { sms: { ...zero, sent: 45, failed: 2 } } } },
    })
    expect(w.findAll('h3').map((h) => h.text())).toEqual(['Varyant A', 'Varyant B'])
    expect(w.text()).toContain('40')
    expect(w.text()).toContain('45')
  })
})
