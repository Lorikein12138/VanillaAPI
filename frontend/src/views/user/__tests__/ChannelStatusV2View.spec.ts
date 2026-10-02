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
  getAdminPresentationDimensions,
  getAdminPresentationSnapshot,
  getAdminPresentationMatrix,
  getAdminPresentationModels,
  getAdminPresentationErrors,
  getAdminPresentationUsers,
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
  getAdminPresentationDimensions: vi.fn(),
  getAdminPresentationSnapshot: vi.fn(),
  getAdminPresentationMatrix: vi.fn(),
  getAdminPresentationModels: vi.fn(),
  getAdminPresentationErrors: vi.fn(),
  getAdminPresentationUsers: vi.fn(),
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
  getAdminPresentationDimensions,
  getAdminPresentationSnapshot,
  getAdminPresentationMatrix,
  getAdminPresentationModels,
  getAdminPresentationErrors,
  getAdminPresentationUsers,
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
      health: { overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy', minimum_sample: 50 },
      trend: [],
    })
    getMatrix.mockResolvedValue({ coverage, group_by: 'platform_group', items: [] })
    getModels.mockResolvedValue({ items: [] })
    getErrors.mockResolvedValue({ items: [] })
    getUsers.mockResolvedValue({ items: [] })
    getAdminPresentationDimensions.mockResolvedValue(dimensions)
    getAdminPresentationSnapshot.mockResolvedValue({
      coverage,
      config: { refresh_interval_seconds: 300 },
      metrics: emptyMetrics,
      health: { overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy', minimum_sample: 50 },
      trend: [],
    })
    getAdminPresentationMatrix.mockResolvedValue({ coverage, group_by: 'platform_group', items: [] })
    getAdminPresentationModels.mockResolvedValue({ items: [] })
    getAdminPresentationErrors.mockResolvedValue({ items: [] })
    getAdminPresentationUsers.mockResolvedValue({ items: [] })
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

  it('reuses one UTC as_of anchor across refresh surfaces and tab changes', async () => {
    route.query = {}
    const wrapper = mountView()
    await flushPromises()

    const asOf = getDimensions.mock.calls[0][3]
    expect(asOf).toMatch(/^\d{4}-\d{2}-\d{2}T.*Z$/)
    expect(getSnapshot.mock.calls[0][3]).toBe(asOf)
    expect(getMatrix.mock.calls[0][4]).toBe(asOf)
    expect(getModels.mock.calls[0][3]).toBe(asOf)

    await wrapper.findAll('[role="tab"]')[1].trigger('click')
    await flushPromises()
    expect(getErrors.mock.calls.at(-1)?.[3]).toBe(asOf)

    await wrapper.findAll('[role="tab"]')[2].trigger('click')
    await flushPromises()
    expect(getUsers.mock.calls.at(-1)?.[3]).toBe(asOf)

    await wrapper.get('[data-testid="change-group-by"]').trigger('click')
    await flushPromises()
    const nextAsOf = getDimensions.mock.calls.at(-1)?.[3]
    expect(nextAsOf).toMatch(/^\d{4}-\d{2}-\d{2}T.*Z$/)
    expect(getSnapshot.mock.calls.at(-1)?.[3]).toBe(nextAsOf)
    expect(getMatrix.mock.calls.at(-1)?.[4]).toBe(nextAsOf)
    expect(getUsers.mock.calls.at(-1)?.[3]).toBe(nextAsOf)
    wrapper.unmount()
  })

  it('uses admin presentation resources on the shared monitor page', async () => {
    authState.isAdmin = true
    route.query = {}
    const wrapper = mountView()
    await flushPromises()

    expect(getAdminPresentationDimensions).toHaveBeenCalled()
    expect(getAdminPresentationSnapshot).toHaveBeenCalled()
    expect(getAdminPresentationMatrix).toHaveBeenCalled()
    expect(getAdminPresentationModels).toHaveBeenCalled()
    expect(getAdminPresentationErrors).not.toHaveBeenCalled()
    expect(getAdminPresentationUsers).not.toHaveBeenCalled()

    await wrapper.findAll('[role="tab"]')[1].trigger('click')
    await flushPromises()
    expect(getAdminPresentationErrors).toHaveBeenCalled()

    await wrapper.findAll('[role="tab"]')[2].trigger('click')
    await flushPromises()
    expect(getAdminPresentationUsers).toHaveBeenCalled()
    expect(getDimensions).not.toHaveBeenCalled()
    expect(getSnapshot).not.toHaveBeenCalled()
    expect(getMatrix).not.toHaveBeenCalled()
    expect(getModels).not.toHaveBeenCalled()
    expect(getErrors).not.toHaveBeenCalled()
    expect(getUsers).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
