import { flushPromises, shallowMount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const {
  route,
  router,
  authState,
  getDimensions,
  getSnapshot,
  getMatrix,
  getModels,
  getErrors,
  getUsers,
  showError,
} = vi.hoisted(() => ({
  route: { query: {} as Record<string, unknown> },
  router: { replace: vi.fn() },
  authState: { isAdmin: false },
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
vi.mock('@/stores/auth', () => ({ useAuthStore: () => authState }))
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
        Select: {
          props: ['modelValue', 'options'],
          template: '<button data-testid="change-group-by" @click="$emit(\'update:modelValue\', \'platform_model\')">change</button>',
        },
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
    authState.isAdmin = false
    route.query = { platform: 'openai' }
    getDimensions.mockResolvedValue(dimensions)
    getSnapshot.mockResolvedValue({
      coverage,
      config: { refresh_interval_seconds: 300 },
      metrics: emptyMetrics,
      health: { overall: 'unknown', error_rate: 'unknown', ttft: 'unknown', cache: 'unknown', minimum_sample: 50 },
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

  it.each([false, true])('uses original reads with admin=%s and no as_of', async (admin) => {
    authState.isAdmin = admin
    route.query = {}
    const wrapper = mountView()
    await flushPromises()
    expect(getDimensions).toHaveBeenCalledWith(expect.objectContaining({ platforms: [], groupIds: [], models: [] }), admin, expect.any(AbortSignal))
    expect(getSnapshot).toHaveBeenCalledWith(expect.any(Object), admin, expect.any(AbortSignal))
    expect(getMatrix).toHaveBeenCalledWith(expect.any(Object), expect.any(String), admin, expect.any(AbortSignal))
    expect(getModels).toHaveBeenCalledWith(expect.any(Object), admin, expect.any(AbortSignal))
    await wrapper.findAll('[role="tab"]')[1].trigger('click')
    await flushPromises()
    expect(getErrors).toHaveBeenCalledWith(expect.any(Object), admin, undefined)
    await wrapper.findAll('[role="tab"]')[2].trigger('click')
    await flushPromises()
    expect(getUsers).toHaveBeenCalledWith(expect.any(Object), admin, undefined)
    for (const read of [getDimensions, getSnapshot, getModels, getErrors, getUsers]) {
      expect(read.mock.calls.every((args) => args.length === 3)).toBe(true)
    }
    expect(getMatrix.mock.calls.every((args) => args.length === 4)).toBe(true)
    expect(showError).not.toHaveBeenCalled()
    await wrapper.get('[data-testid="change-group-by"]').trigger('click')
    await flushPromises()
    expect(getSnapshot.mock.calls.at(-1)?.[1]).toBe(admin)
    wrapper.unmount()
  })
})