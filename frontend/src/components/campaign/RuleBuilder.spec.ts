import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import RuleBuilder from './RuleBuilder.vue'
import type { RuleCatalog, SegmentRule } from '../../api/types'

const catalog: RuleCatalog = {
  fields: [{ name: 'age', type: 'int', operators: ['gte', 'lte', 'between'], min: 0, max: 120, description: 'Age' }, { name: 'language', type: 'language', operators: ['eq', 'in'], description: 'Language' }],
  events: [{ name: 'cart_add', description: 'Cart add', not_followed_by: ['purchase'] }, { name: 'purchase', description: 'Purchase', not_followed_by: [] }],
  languages: ['tr', 'en'],
  places: [{ level: 'country', name: 'Türkiye', code: 'TR', country: 'TR' }, { level: 'city', name: 'Berlin', geoname_id: 2950159, country: 'DE' }],
  max_conditions: 10,
  max_within_days: 365,
  max_min_count: 100,
  max_list_values: 20,
}

describe('RuleBuilder', () => {
  it('offers city and country but never a region from a stale catalog', async () => {
    const withCollision: RuleCatalog = { ...catalog, places: [
      { level: 'country', name: 'Fransa', code: 'FR', country: 'FR' },
      { level: 'admin1', name: 'Île-de-France', code: 'FR-IDF', country: 'FR' },
      { level: 'city', name: 'Paris', geoname_id: 2988507, country: 'FR' },
      { level: 'city', name: 'Paris', geoname_id: 4717560, country: 'US' },
    ] }
    const w = mount(RuleBuilder, { props: { modelValue: { match: 'all', conditions: [{ kind: 'location', operator: 'in', places: [] }] }, catalog: withCollision } })
    const options = w.get('[data-test="location-place-0"]').findAll('option').map((o) => o.attributes('value'))
    expect(options).toEqual(['', 'country:FR:FR', 'city:FR:2988507', 'city:US:4717560'])
    await w.get('[data-test="location-place-0"]').setValue(options[2]!)
    const changed = w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule
    expect(changed.conditions[0]?.places?.[0]).toMatchObject({ country: 'FR', name: 'Paris' })
    expect(w.get('[data-test="location-search-0"]').attributes('placeholder')).toBe('Şehir veya ülke')
  })
  it('allows typing a comma before the next language in a list', async () => {
    const rule: SegmentRule = { match: 'all', conditions: [{ kind: 'attribute', field: 'language', operator: 'in', values: ['tr'] }] }
    const w = mount(RuleBuilder, { props: { modelValue: rule, catalog } })
    const input = w.get('[data-test="attribute-value-0-0"]')
    ;(input.element as HTMLInputElement).value = 'tr,'
    await input.trigger('input')
    expect((input.element as HTMLInputElement).value).toBe('tr,')
    ;(input.element as HTMLInputElement).value = 'tr,en'
    await input.trigger('change')
    expect((w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule).conditions[0]?.values).toEqual(['tr', 'en'])
  })
  it('keeps a generated event sequence intact while editing another condition', async () => {
    const rule: SegmentRule = { match: 'all', conditions: [
      { kind: 'event', event_type: 'cart_add', within_days: 30, not_followed_by: 'purchase', min_count: 1 },
      { kind: 'attribute', field: 'age', operator: 'gte', values: ['18'] },
    ] }
    const w = mount(RuleBuilder, { props: { modelValue: rule, catalog } })
    await w.get('[data-test="attribute-value-1-0"]').setValue('21')
    const changed = w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule
    expect(changed.conditions[0]).toMatchObject({ event_type: 'cart_add', within_days: 30, not_followed_by: 'purchase' })
    expect(changed.conditions[1]?.values).toEqual(['21'])
  })

  it('keeps each place search with its condition when an earlier one is removed', async () => {
    const rule: SegmentRule = { match: 'all', conditions: [
      { kind: 'location', operator: 'in', places: [] },
      { kind: 'location', operator: 'in', places: [] },
    ] }
    const w = mount(RuleBuilder, { props: { modelValue: rule, catalog } })
    await w.get('[data-test="location-search-1"]').setValue('Ber')
    await w.get('[data-test="remove-condition-0"]').trigger('click')
    await w.setProps({ modelValue: w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule })
    expect((w.get('[data-test="location-search-0"]').element as HTMLInputElement).value).toBe('Ber')
    expect(w.get('[data-test="location-place-0"]').findAll('option').map((o) => o.text())).toEqual(['Seçin', 'Berlin (şehir, DE)'])
  })

  it('lets a marketer choose all/any, a canonical place and remove a condition', async () => {
    const w = mount(RuleBuilder, { props: { modelValue: { match: 'all', conditions: [] }, catalog } })
    await w.get('[data-test="rule-match"]').setValue('any')
    await w.setProps({ modelValue: w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule })
    await w.get('[data-test="add-condition"]').trigger('click')
    await w.setProps({ modelValue: w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule })
    await w.get('[data-test="condition-kind-0"]').setValue('location')
    await w.setProps({ modelValue: w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule })
    await w.get('[data-test="location-place-0"]').setValue('city:DE:2950159')
    const changed = w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule
    expect(changed).toMatchObject({ match: 'any', conditions: [{ kind: 'location', operator: 'in', places: [{ level: 'city', geoname_id: 2950159, country: 'DE' }] }] })
    await w.setProps({ modelValue: changed })
    await w.get('[data-test="remove-condition-0"]').trigger('click')
    expect((w.emitted('update:modelValue')?.at(-1)?.[0] as SegmentRule).conditions).toEqual([])
  })
})
