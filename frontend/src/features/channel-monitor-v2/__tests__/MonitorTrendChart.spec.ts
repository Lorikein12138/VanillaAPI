import { describe, expect, it, vi } from 'vitest'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      const labels: Record<string, string> = {
        'channelMonitorV2.chart.errorDataset': 'Error rate',
        'channelMonitorV2.chart.cacheDataset': 'Cache rate',
        'channelMonitorV2.chart.ttftDataset': 'TTFT',
      }
      return labels[key] || key.replace('{count}', String(params?.count ?? ''))
    },
    locale: { value: 'en' },
  }),
}))

vi.mock('vue-chartjs', () => ({
  Line: { name: 'Line', props: ['data', 'options'], template: '<div data-testid="trend-line" />' },
}))

import { mount } from '@vue/test-utils'
import MonitorTrendChart from '../MonitorTrendChart.vue'
import type { MonitorCoverage, MonitorHealth, MonitorMetric } from '@/api/channelMonitorV2'

const coverage: MonitorCoverage = {
  requested_start: '2026-09-28T00:00:00Z',
  requested_end: '2026-09-29T00:00:00Z',
  coverage_start: '2026-09-28T00:00:00Z',
  data_through: '2026-09-29T00:00:00Z',
  computed_at: '2026-09-29T00:00:00Z',
  aggregation_lag_seconds: 0,
  coverage_complete: true,
  bucket_seconds: 3600,
}

const health: MonitorHealth = {
  overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy',
  score: 90, error_rate_score: 90, ttft_score: 90, cache_score: 90, minimum_sample: 20,
}

function metric(errorRate: number, cacheRate: number, ttftMs: number): MonitorMetric {
  return {
    success_requests: 90, error_requests: 10, request_count: 100, token_count: 1000,
    rpm: 2, tpm: 20, error_rate: errorRate, success_rate: 1 - errorRate,
    cache_rate: cacheRate, cache_rate_numerator: 900, cache_rate_denominator: 1000,
    ttft: { sample_count: 90, p50_ms: ttftMs, p90_ms: ttftMs + 500, p95_ms: ttftMs + 700, avg_ms: ttftMs + 200 },
    duration: { sample_count: 100, p50_ms: 12000, p90_ms: 13000, p95_ms: 14000, avg_ms: 12500 },
  }
}

describe('MonitorTrendChart', () => {
  it('draws smoothed series while tooltips report the unmodified projection source points', () => {
    const trend = [
      { bucket_start: '2026-09-28T00:00:00Z', metrics: metric(0, .8, 1000), health },
      { bucket_start: '2026-09-28T01:00:00Z', metrics: metric(.3, .9, 2000), health },
      { bucket_start: '2026-09-28T02:00:00Z', metrics: metric(.3, .8, 4000), health },
    ]
    const source = structuredClone(trend)
    const wrapper = mount(MonitorTrendChart, {
      props: { trend, coverage },
      global: { stubs: { Icon: true, EmptyState: true } },
    })
    const chart = wrapper.getComponent({ name: 'Line' })
    const datasets = (chart.props('data') as { datasets: Array<{ data: number[] }> }).datasets

    expect(datasets[0].data).toEqual([15, 20, 30])
    expect(datasets[1].data[1]).toBeCloseTo(83.333333, 5)
    expect(datasets[2].data[1]).toBeCloseTo(2333.333333, 5)
    const options = chart.props('options') as { plugins: { tooltip: { callbacks: { label: (context: unknown) => string } } } }
    const label = options.plugins.tooltip.callbacks.label
    expect(label({ dataset: { label: 'Error rate' }, parsed: { y: 20 }, dataIndex: 1 })).toBe('Error rate: 30.0%')
    expect(label({ dataset: { label: 'Cache rate' }, parsed: { y: 83.333 }, dataIndex: 1 })).toBe('Cache rate: 90.0%')
    expect(label({ dataset: { label: 'TTFT' }, parsed: { y: 2333.333 }, dataIndex: 1 })).toContain('2.0s')
    expect(trend).toEqual(source)
    expect(wrapper.text().toLowerCase()).not.toMatch(/synthetic|simulat|fake|伪造|模拟/)
    expect(wrapper.html().toLowerCase()).not.toMatch(/synthetic|simulat|fake|伪造|模拟/)
  })
})
