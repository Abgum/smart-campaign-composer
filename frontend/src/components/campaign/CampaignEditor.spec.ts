import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import CampaignEditor from './CampaignEditor.vue'
import type { RuleCatalog, Version } from '../../api/types'

const catalog: RuleCatalog = {
  fields: [{ name: 'age', type: 'int', operators: ['gte'], min: 0, max: 120, description: 'Age' }],
  events: [{ name: 'cart_add', description: 'Cart', not_followed_by: ['purchase'] }], languages: ['tr', 'en'],
  places: [], max_conditions: 10, max_within_days: 365, max_min_count: 100, max_list_values: 20,
}
const version: Version = {
  id: 4, version: 2, source: 'llm', llm_provider: 'mock', name: 'Sepet',
  segment_rule: { match: 'all', conditions: [{ kind: 'event', event_type: 'cart_add', within_days: 30, min_count: 1, not_followed_by: 'purchase' }] },
  sms: { text: 'Merhaba {{first_name}}' }, email: { subject: 'Sepet', body: 'Gel' },
  change_summary: '', created_at: '2026-10-28T10:00:00Z',
}

describe('CampaignEditor', () => {
  it('saves all editable fields and preserves the generated audience sequence', async () => {
    const w = mount(CampaignEditor, { props: { initial: version, catalog } })
    await w.get('[data-test="campaign-name"]').setValue('Sepet fırsatı')
    await w.get('[data-test="sms-text"]').setValue('Merhaba, fırsat burada')
    await w.get('[data-test="email-subject"]').setValue('Yeni fırsat')
    await w.get('form.campaign-editor').trigger('submit')
    expect(w.emitted('save')?.[0]?.[0]).toMatchObject({
      name: 'Sepet fırsatı', sms: { text: 'Merhaba, fırsat burada' }, email: { subject: 'Yeni fırsat', body: 'Gel' },
      segment_rule: { conditions: [{ event_type: 'cart_add', not_followed_by: 'purchase', within_days: 30 }] },
    })
  })

  it('can start without AI, requiring a rule and at least one channel before save', async () => {
    const w = mount(CampaignEditor, { props: { initial: null, catalog } })
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeDefined()
    await w.get('[data-test="campaign-name"]').setValue('Yeni kampanya')
    await w.get('[data-test="add-condition"]').trigger('click')
    await w.get('[data-test="attribute-value-0-0"]').setValue('18')
    await w.get('[data-test="sms-on"]').setValue(true)
    await w.get('[data-test="sms-text"]').setValue('Merhaba')
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeUndefined()
    await w.get('form.campaign-editor').trigger('submit')
    expect(w.emitted('save')?.[0]?.[0]).toMatchObject({ name: 'Yeni kampanya', sms: { text: 'Merhaba' }, segment_rule: { conditions: [{ field: 'age', values: ['18'] }] } })
  })

  it('requires every selected channel to have complete content', async () => {
    const w = mount(CampaignEditor, { props: { initial: null, catalog } })
    await w.get('[data-test="campaign-name"]').setValue('İki kanal')
    await w.get('[data-test="add-condition"]').trigger('click')
    await w.get('[data-test="attribute-value-0-0"]').setValue('18')
    await w.get('[data-test="sms-on"]').setValue(true)
    await w.get('[data-test="sms-text"]').setValue('Merhaba')
    await w.get('[data-test="email-on"]').setValue(true)
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeDefined()
    await w.get('[data-test="email-subject"]').setValue('Konu')
    await w.get('[data-test="email-body"]').setValue('Gövde')
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeUndefined()
  })

  // The server counts a variable as 10 characters; the editor agrees with
  // it and with the preview.
  it('counts lengths as the server does, a variable as 10 characters', async () => {
    const w = mount(CampaignEditor, { props: { initial: null, catalog } })
    await w.get('[data-test="campaign-name"]').setValue('Uzun SMS')
    await w.get('[data-test="add-condition"]').trigger('click')
    await w.get('[data-test="attribute-value-0-0"]').setValue('18')
    await w.get('[data-test="sms-on"]').setValue(true)
    await w.get('[data-test="sms-text"]').setValue('a'.repeat(150) + '{{first_name}}')
    expect(w.get('.counter').text()).toContain('160/160')
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeUndefined()
    await w.get('[data-test="sms-text"]').setValue('a'.repeat(151) + '{{ first_name }}')
    expect(w.get('.counter').text()).toContain('161/160')
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeDefined()
  })

  it('does not save a blank SMS when email content is complete', async () => {
    const w = mount(CampaignEditor, { props: { initial: null, catalog } })
    await w.get('[data-test="campaign-name"]').setValue('İki kanal')
    await w.get('[data-test="add-condition"]').trigger('click')
    await w.get('[data-test="attribute-value-0-0"]').setValue('18')
    await w.get('[data-test="sms-on"]').setValue(true)
    await w.get('[data-test="email-on"]').setValue(true)
    await w.get('[data-test="email-subject"]').setValue('Konu')
    await w.get('[data-test="email-body"]').setValue('Gövde')
    expect(w.get('[data-test="save-draft"]').attributes('disabled')).toBeDefined()
  })
})
