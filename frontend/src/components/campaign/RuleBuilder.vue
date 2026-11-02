<script setup lang="ts">
import { t, tk } from '../../i18n'
import { computed, ref } from 'vue'
import type { Condition, PlaceRef, RuleCatalog, SegmentRule } from '../../api/types'

const props = defineProps<{ modelValue: SegmentRule; catalog: RuleCatalog }>()
const emit = defineEmits<{ 'update:modelValue': [value: SegmentRule] }>()
const search = ref<Record<number, string>>({})

const labels: Record<string, string> = {
  age: tk('Yaş'), total_orders: tk('Toplam sipariş'), signup_days_ago: tk('Üyelikten beri geçen gün'),
  total_spent: tk('Toplam harcama'), language: tk('Dil'), cart_add: tk('Sepete ekleme'),
  purchase: tk('Satın alma'), product_view: tk('Ürün görüntüleme'), eq: tk('Eşittir'), gte: tk('En az'),
  lte: tk('En çok'), between: tk('Arasında'), in: tk('Biri'), not_in: tk('Hiçbiri'),
}
const label = (key: string) => (labels[key] ? t(labels[key]) : key)
const field = (c: Condition) => props.catalog.fields.find((f) => f.name === c.field)
const event = (c: Condition) => props.catalog.events.find((e) => e.name === c.event_type)
const placeKey = (p: PlaceRef) => `${p.level}:${p.country ?? p.code}:${p.level === 'city' ? p.geoname_id : p.code}`
const placeLevel = (p: PlaceRef) => (p.level === 'country' ? t('ülke') : p.level === 'admin1' ? t('il/bölge') : t('şehir'))
const placeLabel = (p: PlaceRef) => `${p.name ?? p.code} (${placeLevel(p)}, ${p.country ?? p.code})`
const availablePlaces = (i: number) => {
  const q = (search.value[i] ?? '').toLocaleLowerCase('tr')
  return props.catalog.places.filter((p) => p.level !== 'admin1' && (!q || placeLabel(p).toLocaleLowerCase('tr').includes(q)))
}
const count = computed(() => props.modelValue.conditions.length)

function copy(): SegmentRule {
  return {
    match: props.modelValue.match,
    conditions: props.modelValue.conditions.map((c) => ({
      ...c,
      values: c.values ? [...c.values] : undefined,
      places: c.places ? c.places.map((p) => ({ ...p })) : undefined,
    })),
  }
}
function replace(rule: SegmentRule) { emit('update:modelValue', rule) }
function change(i: number, update: Partial<Condition>) {
  const next = copy()
  next.conditions[i] = { ...next.conditions[i], ...update } as Condition
  replace(next)
}
function kind(i: number, nextKind: Condition['kind']) {
  const next = copy()
  next.conditions[i] = nextKind === 'location'
    ? { kind: 'location', operator: 'in', places: [] }
    : nextKind === 'event'
      ? { kind: 'event', event_type: props.catalog.events[0]?.name ?? '', within_days: 30, min_count: 1 }
      : { kind: 'attribute', field: props.catalog.fields[0]?.name ?? '', operator: props.catalog.fields[0]?.operators[0] ?? 'eq', values: [''] }
  replace(next)
}
function add() {
  if (count.value >= props.catalog.max_conditions) return
  const next = copy()
  next.conditions.push({ kind: 'attribute', field: props.catalog.fields[0]?.name ?? '', operator: props.catalog.fields[0]?.operators[0] ?? 'eq', values: [''] })
  replace(next)
}
function remove(i: number) {
  const next = copy()
  next.conditions.splice(i, 1)
  // A place search belongs to its condition, not to a position: the ones
  // after the removed condition move up with it.
  const moved: Record<number, string> = {}
  for (const [k, text] of Object.entries(search.value)) {
    const at = Number(k)
    if (at !== i) moved[at < i ? at : at - 1] = text
  }
  search.value = moved
  replace(next)
}
function attributeField(i: number, name: string) {
  const spec = props.catalog.fields.find((f) => f.name === name)
  change(i, { field: name, operator: spec?.operators[0] ?? 'eq', values: [''] })
}
function attributeOperator(i: number, op: string) {
  change(i, { operator: op, values: op === 'between' ? ['', ''] : [''] })
}
function attributeValue(i: number, j: number, value: string) {
  const values = [...(props.modelValue.conditions[i]?.values ?? [])]
  if (props.modelValue.conditions[i]?.operator === 'in') {
    change(i, { values: value.split(',').map((v) => v.trim()).filter(Boolean) })
    return
  }
  values[j] = value
  change(i, { values })
}
function addPlace(i: number, key: string) {
  const place = props.catalog.places.find((p) => placeKey(p) === key)
  if (!place) return
  const current = props.modelValue.conditions[i]?.places ?? []
  if (current.some((p) => placeKey(p) === key) || current.length >= props.catalog.max_list_values) return
  change(i, { places: [...current, place] })
}
function removePlace(i: number, key: string) {
  change(i, { places: (props.modelValue.conditions[i]?.places ?? []).filter((p) => placeKey(p) !== key) })
}
</script>

<template>
  <fieldset class="rule-builder">
    <legend>{{ t('Hedef kitle kuralı') }}</legend>
    <p class="muted small">{{ t('Koşulları seçin; kişi sayısı kaydedince yeniden hesaplanır. İzinler gönderimden önce ayrıca kontrol edilir.') }}</p>
    <label class="field">{{ t('Koşullar nasıl birleşsin?') }}
      <select data-test="rule-match" :value="modelValue.match" @change="replace({ ...copy(), match: ($event.target as HTMLSelectElement).value as SegmentRule['match'] })">
        <option value="all">{{ t('Tüm koşullar sağlansın') }}</option><option value="any">{{ t('Koşullardan biri sağlansın') }}</option>
      </select>
    </label>
    <div v-for="(c, i) in modelValue.conditions" :key="i" class="rule-condition">
      <div class="rule-condition-head">
        <label>{{ t('Koşul {n}', { n: i + 1 }) }}
          <select :data-test="`condition-kind-${i}`" :value="c.kind" @change="kind(i, ($event.target as HTMLSelectElement).value as Condition['kind'])">
            <option value="attribute">{{ t('Kullanıcı özelliği') }}</option><option value="location">{{ t('Konum') }}</option><option value="event">{{ t('Davranış') }}</option>
          </select>
        </label>
        <button type="button" :data-test="`remove-condition-${i}`" :aria-label="t('Koşul {n} sil', { n: i + 1 })" @click="remove(i)">{{ t('Sil') }}</button>
      </div>
      <template v-if="c.kind === 'attribute'">
        <label>{{ t('Alan') }}
          <select :value="c.field" :data-test="`attribute-field-${i}`" @change="attributeField(i, ($event.target as HTMLSelectElement).value)">
            <option v-for="f in catalog.fields" :key="f.name" :value="f.name">{{ label(f.name) }}</option>
          </select>
        </label>
        <label>{{ t('Karşılaştırma') }}
          <select :value="c.operator" :data-test="`attribute-operator-${i}`" @change="attributeOperator(i, ($event.target as HTMLSelectElement).value)">
            <option v-for="op in field(c)?.operators ?? []" :key="op" :value="op">{{ label(op) }}</option>
          </select>
        </label>
        <template v-if="field(c)?.type === 'language' && c.operator !== 'in'">
          <label>{{ t('Dil') }}
            <select :value="c.values?.[0] ?? ''" :data-test="`attribute-value-${i}-0`" @change="attributeValue(i, 0, ($event.target as HTMLSelectElement).value)">
              <option value="">{{ t('Seçin') }}</option><option v-for="lang in catalog.languages" :key="lang" :value="lang">{{ lang }}</option>
            </select>
          </label>
        </template>
        <template v-else>
          <label v-for="j in c.operator === 'between' ? 2 : 1" :key="j">{{ c.operator === 'between' ? (j === 1 ? t('Alt sınır') : t('Üst sınır')) : c.operator === 'in' ? t('Virgülle ayrılmış değerler') : t('Değer') }}
            <input :data-test="`attribute-value-${i}-${j - 1}`" :type="field(c)?.type === 'language' || c.operator === 'in' ? 'text' : 'number'" :min="field(c)?.min" :max="field(c)?.max" :step="field(c)?.type === 'decimal' ? '0.01' : '1'" :value="c.operator === 'in' ? c.values?.join(', ') : c.values?.[j - 1] ?? ''" @input="c.operator !== 'in' && attributeValue(i, j - 1, ($event.target as HTMLInputElement).value)" @change="c.operator === 'in' && attributeValue(i, j - 1, ($event.target as HTMLInputElement).value)" />
          </label>
        </template>
      </template>
      <template v-else-if="c.kind === 'location'">
        <label>{{ t('Konum ilişkisi') }}
          <select :value="c.operator" @change="change(i, { operator: ($event.target as HTMLSelectElement).value })"><option value="in">{{ t('Bu konumlardan biri') }}</option><option value="not_in">{{ t('Bu konumların dışında') }}</option></select>
        </label>
        <div class="chosen-places" :aria-label="t('Seçili konumlar')">
          <span v-for="p in c.places ?? []" :key="placeKey(p)" class="chip">{{ placeLabel(p) }} <button type="button" :aria-label="t('{name} kaldır', { name: placeLabel(p) })" @click="removePlace(i, placeKey(p))">×</button></span>
        </div>
        <label>{{ t('Konum ara') }}<input v-model="search[i]" type="search" :data-test="`location-search-${i}`" :placeholder="t('Şehir veya ülke')" /></label>
        <label>{{ t('Konum ekle') }}
          <select :data-test="`location-place-${i}`" value="" @change="addPlace(i, ($event.target as HTMLSelectElement).value); ($event.target as HTMLSelectElement).value = ''">
            <option value="">{{ t('Seçin') }}</option><option v-for="p in availablePlaces(i)" :key="placeKey(p)" :value="placeKey(p)">{{ placeLabel(p) }}</option>
          </select>
        </label>
      </template>
      <template v-else-if="c.kind === 'event'">
        <label>{{ t('Davranış') }}
          <select :value="c.event_type" :data-test="`event-type-${i}`" @change="change(i, { event_type: ($event.target as HTMLSelectElement).value, not_followed_by: undefined })">
            <option v-for="e in catalog.events" :key="e.name" :value="e.name">{{ label(e.name) }}</option>
          </select>
        </label>
        <label>{{ t('Son kaç gün?') }}<input :value="c.within_days ?? 30" type="number" min="1" :max="catalog.max_within_days" :data-test="`event-days-${i}`" @input="change(i, { within_days: Number(($event.target as HTMLInputElement).value) })" /></label>
        <label>{{ t('En az kaç kez?') }}<input :value="c.min_count ?? 1" type="number" min="1" :max="catalog.max_min_count" :disabled="!!c.not_followed_by" @input="change(i, { min_count: Number(($event.target as HTMLInputElement).value) })" /></label>
        <label class="toggle"><input type="checkbox" :checked="!!c.negate" :disabled="!!c.not_followed_by" @change="change(i, { negate: ($event.target as HTMLInputElement).checked })" /> {{ t('Bu davranışı yapmamış olsun') }}</label>
        <label v-if="event(c)?.not_followed_by.length">{{ t('Ardından yapmamış olsun') }}
          <select :value="c.not_followed_by ?? ''" :data-test="`event-followed-by-${i}`" @change="change(i, { not_followed_by: ($event.target as HTMLSelectElement).value || undefined, negate: false, min_count: 1 })">
            <option value="">{{ t('Fark etmez') }}</option><option v-for="e in event(c)?.not_followed_by ?? []" :key="e" :value="e">{{ label(e) }}</option>
          </select>
        </label>
      </template>
    </div>
    <button type="button" class="button" data-test="add-condition" :disabled="count >= catalog.max_conditions" @click="add">{{ t('+ Koşul ekle') }}</button>
    <span class="muted small">{{ t('{n}/{max} koşul', { n: count, max: catalog.max_conditions }) }}</span>
  </fieldset>
</template>
