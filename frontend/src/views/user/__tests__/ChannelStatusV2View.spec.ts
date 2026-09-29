import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { route, router, getDimensions, getSnapshot, getMatrix, getModels, getErrors, getUsers, showError } = vi.hoisted(() => ({
  route: { query: {} as Record<string, unknown> },
  router: { replace: vi.fn() },
  getDimensions: vi.fn(),
  getSnapshot: vi.fn(),
  getMatrix: vi.fn(),
  getModels: vi.fn(),
  getErrors: vi.fn(),
  getUsers: vi.fn(),
  showError: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => route,
  useRouter: () => router,
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAdmin: false }) }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError }) }))
vi.mock('@/utils/featureFlags', () => ({
  isChannelMonitorThroughputHidden: () => false,
  isChannelMonitorUserRankingHidden: () => false,
}))
vi.mock('@/api/channelMonitorV2', () => ({
  getDimensions,
  getSnapshot,
  getMatrix,
  getModels,
  getErrors,
  getUsers,
}))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key, te: () => true, locale: { value: 'en' } }),
}))

import ChannelStatusV2View from '../ChannelStatusV2View.vue'

const emptyMetrics = {
  success_requests: 0,
  error_requests: 0,
  request_count: 0,
  token_count: 0,
  rpm: 0,
  tpm: 0,
  error_rate: 0,
  success_rate: 0,
  cache_rate: 0,
  cache_rate_numerator: 0,
  cache_rate_denominator: 0,
  ttft: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
  duration: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
}

const coverage = {
  requested_start: '2026-09-28T00:00:00Z',
  requested_end: '2026-09-28T00:05:00Z',
  coverage_start: '2026-09-28T00:00:00Z',
  data_through: '2026-09-28T00:05:00Z',
  computed_at: '2026-09-28T00:05:00Z',
  aggregation_lag_seconds: 0,
  coverage_complete: true,
  bucket_seconds: 300,
}

const dimensions = {
  platforms: [
    { value: 'openai', label: 'openai', request_count: 0 },
    { value: 'gemini', label: 'gemini', request_count: 0 },
  ],
  groups: [
    { id: 1, name: 'Composite group', platform: 'composite', platforms: ['openai', 'gemini'], request_count: 0 },
    { id: 2, name: 'OpenAI group', platform: 'openai', request_count: 0 },
  ],
  models: [],
}

function mountView() {
  return shallowMount(ChannelStatusV2View, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        FilterMultiSelect: {
          props: ['options'],
          template: '<div data-testid="filter-options"><span v-for="option in options" :key="option.value">{{ option.value }}</span></div>',
        },
      },
    },
  })
}

describe('ChannelStatusV2View dimension filters', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    route.query = { platform: 'openai' }
    getDimensions.mockResolvedValue(dimensions)
    getSnapshot.mockResolvedValue({
      coverage,
      config: { refresh_interval_seconds: 300 },
      metrics: emptyMetrics,
      health: { overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy', minimum_sample: 50 },
      trend: [],
    })
    getMatrix.mockResolvedValue({ coverage, group_by: 'platform_group', items: [] })
    getModels.mockResolvedValue({ items: [] })
    getErrors.mockResolvedValue({ items: [] })
    getUsers.mockResolvedValue({ items: [] })
  })

  it('keeps a composite group in the picker when one of its concrete platforms is selected', async () => {
    const wrapper = mountView()
    await flushPromises()

    const filterOptions = wrapper.findAll('[data-testid="filter-options"]')
    expect(filterOptions[1].text()).toBe('12')
    wrapper.unmount()
  })

  it('clears stale Kiro and group URL selections after an empty group catalog loads', async () => {
    route.query = { platform: 'kiro', group: '77' }
    getDimensions.mockResolvedValue({ platforms: dimensions.platforms, groups: [], models: [] })
    const wrapper = mountView()
    await flushPromises()
    await flushPromises()

    expect(getSnapshot.mock.calls.some(([latestFilter]) =>
      latestFilter.platforms.length === 0 && latestFilter.groupIds.length === 0,
    )).toBe(true)
    expect(router.replace).toHaveBeenCalledWith(expect.objectContaining({
      query: expect.objectContaining({ platform: undefined, group: undefined }),
    }))
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
