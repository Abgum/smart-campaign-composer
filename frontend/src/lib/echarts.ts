import * as echarts from 'echarts/core'
import { BarChart, type BarSeriesOption } from 'echarts/charts'
import { GridComponent, TooltipComponent, type GridComponentOption, type TooltipComponentOption } from 'echarts/components'
import { SVGRenderer } from 'echarts/renderers'

// Only the parts the charts use, so the bundle carries no more of ECharts
// than that. SVG keeps the marks sharp at any zoom.
echarts.use([BarChart, GridComponent, TooltipComponent, SVGRenderer])

export type ChartOption = echarts.ComposeOption<BarSeriesOption | GridComponentOption | TooltipComponentOption>
export { echarts }
