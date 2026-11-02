import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

const { chart, init } = vi.hoisted(() => {
  const chart = { setOption: vi.fn(), resize: vi.fn(), dispose: vi.fn(), dispatchAction: vi.fn() }
  return { chart, init: vi.fn(() => chart) }
})
vi.mock('../../lib/echarts', () => ({ echarts: { init } }))

import EChart from './EChart.vue'

describe('EChart', () => {
  let width = 600
  beforeEach(() => {
    width = 600
    vi.spyOn(Element.prototype, 'clientWidth', 'get').mockImplementation(() => width)
  })
  afterEach(() => vi.clearAllMocks())

  it('draws the option as SVG, redraws it whole when it changes, and lets go when removed', async () => {
    const w = mount(EChart, { props: { option: { series: [] }, height: 240, label: 'Günlük gönderim' } })
    const box = w.get('.echart')
    expect(init).toHaveBeenCalledWith(box.element, null, { renderer: 'svg' })
    expect(chart.setOption).toHaveBeenCalledWith({ series: [] })
    expect(box.attributes('role')).toBe('img')
    expect(box.attributes('aria-label')).toBe('Günlük gönderim')
    expect(box.attributes('style')).toContain('height: 240px')
    expect(box.attributes('tabindex')).toBeUndefined() // nothing to step through

    await w.setProps({ option: { series: [{ type: 'bar' }] } })
    expect(chart.setOption).toHaveBeenLastCalledWith({ series: [{ type: 'bar' }] }, { notMerge: true })

    w.unmount()
    expect(chart.dispose).toHaveBeenCalled()
  })

  it('steps through the columns by keyboard, showing each one and saying it', async () => {
    const describe = (i: number) => `sütun ${i + 1}`
    const w = mount(EChart, { props: { option: { series: [] }, height: 240, label: 'Günlük gönderim', items: 3, describe } })
    const box = w.get('.echart')
    expect(box.attributes('tabindex')).toBe('0')
    expect(box.attributes('aria-label')).toContain('ok tuşları')
    const live = () => w.get('.chart-live').text()
    await box.trigger('focus')
    expect(chart.dispatchAction).toHaveBeenLastCalledWith({ type: 'showTip', seriesIndex: 0, dataIndex: 0 })
    expect(live()).toBe('sütun 1')
    await box.trigger('keydown', { key: 'ArrowRight' })
    expect(chart.dispatchAction).toHaveBeenLastCalledWith({ type: 'showTip', seriesIndex: 0, dataIndex: 1 })
    expect(live()).toBe('sütun 2')
    await box.trigger('keydown', { key: 'End' })
    expect(live()).toBe('sütun 3')
    await box.trigger('keydown', { key: 'ArrowRight' }) // stays on the last
    expect(live()).toBe('sütun 3')
    await box.trigger('keydown', { key: 'Home' })
    expect(live()).toBe('sütun 1')
    await box.trigger('keydown', { key: 'ArrowDown' }) // rows of a bar chart go down
    expect(live()).toBe('sütun 2')
    await box.trigger('keydown', { key: 'Escape' })
    expect(chart.dispatchAction).toHaveBeenLastCalledWith({ type: 'hideTip' })
  })

  it('draws without animation when the system asks for less motion', () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: q.includes('reduce'), addEventListener() {}, removeEventListener() {} }))
    mount(EChart, { props: { option: { series: [] }, height: 240, label: 'Günlük gönderim' } })
    expect(chart.setOption).toHaveBeenCalledWith({ series: [], animation: false })
    vi.unstubAllGlobals()
  })

  it('waits for a box with a width before drawing', () => {
    width = 0
    mount(EChart, { props: { option: { series: [] }, height: 240, label: 'Günlük gönderim' } })
    expect(init).not.toHaveBeenCalled()
  })
})
