<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { echarts, type ChartOption } from '../../lib/echarts'
import { reducedMotion } from '../../lib/motion'
import { t } from '../../i18n'

// One ECharts chart on a box of the given height and the width of its
// parent. It draws once the box has a width (a hidden tab has none), redraws
// whole when the option changes, and follows the box when it resizes.
//
// With items and describe it also reads by keyboard: the chart takes focus,
// the arrow keys (Home, End) step through its columns or rows, each step
// shows that tooltip and says describe(i) to screen readers; Esc hides it.
// It draws without animation when the system asks for less motion.
const props = defineProps<{ option: ChartOption; height: number; label: string; items?: number; describe?: (i: number) => string }>()
const el = ref<HTMLDivElement | null>(null)
let chart: ReturnType<typeof echarts.init> | null = null
let observer: ResizeObserver | null = null

const withMotion = (option: ChartOption): ChartOption => (reducedMotion() ? { ...option, animation: false } : option)

function draw() {
  if (chart || !el.value || el.value.clientWidth === 0) return
  chart = echarts.init(el.value, null, { renderer: 'svg' })
  chart.setOption(withMotion(props.option))
}

const keyboard = computed(() => (props.items ?? 0) > 0)
const name = computed(() => (keyboard.value ? `${props.label} ${t('Değerleri ok tuşlarıyla tek tek okuyabilirsiniz.')}` : props.label))
const at = ref(-1)
const spoken = ref('')

function show(i: number) {
  const n = props.items ?? 0
  if (n === 0) return
  at.value = Math.min(Math.max(i, 0), n - 1)
  chart?.dispatchAction({ type: 'showTip', seriesIndex: 0, dataIndex: at.value })
  spoken.value = props.describe?.(at.value) ?? ''
}
const hide = () => chart?.dispatchAction({ type: 'hideTip' })
const steps: Record<string, (i: number) => number> = {
  ArrowRight: (i) => i + 1,
  ArrowDown: (i) => i + 1,
  ArrowLeft: (i) => i - 1,
  ArrowUp: (i) => i - 1,
  Home: () => 0,
  End: () => (props.items ?? 1) - 1,
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    hide()
    return
  }
  const step = steps[e.key]
  if (!step) return
  e.preventDefault()
  show(step(Math.max(at.value, 0)))
}

onMounted(() => {
  draw()
  if (el.value && typeof ResizeObserver !== 'undefined') {
    observer = new ResizeObserver(() => (chart ? chart.resize() : draw()))
    observer.observe(el.value)
  }
})
watch(
  () => props.option,
  (option) => {
    chart?.setOption(withMotion(option), { notMerge: true })
    at.value = -1
  },
)
watch(
  () => props.height,
  () => nextTick(() => chart?.resize()),
)
onBeforeUnmount(() => {
  observer?.disconnect()
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div class="echart-box">
    <div
      ref="el"
      class="echart"
      role="img"
      :aria-label="name"
      :style="{ height: `${height}px` }"
      :tabindex="keyboard ? 0 : undefined"
      @focus="show(Math.max(at, 0))"
      @blur="hide"
      @keydown="onKey"
    />
    <p v-if="keyboard" class="chart-live sr-only" aria-live="polite">{{ spoken }}</p>
  </div>
</template>
