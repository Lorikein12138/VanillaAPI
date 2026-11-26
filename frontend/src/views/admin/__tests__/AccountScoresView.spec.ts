import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AdaptiveAccountScoresPage, AdaptiveScoreMember, AdaptiveScoreV1 } from '@/api/admin/groups'
import accountScores from '@/i18n/locales/en/admin/accountScores'
import { useAdminSettingsStore } from '@/stores/adminSettings'
import AccountScoresView from '../AccountScoresView.vue'

const api = vi.hoisted(() => ({
  groups: vi.fn(), scores: vi.fn(), settings: vi.fn(), payment: vi.fn(),
}))

vi.mock('@/api', () => ({
  adminAPI: {
    groups: { list: api.groups, getAdaptiveAccountScores: api.scores },
    settings: { getSettings: api.settings },
    payment: { getConfig: api.payment },
  },
}))

const observedAt = '2026-11-26T08:03:00Z'
const bucket: AdaptiveScoreV1 = {
  model: 'recent-sol',
  operation: 'POST /v1/responses',
  transport: 'sse',
  tier: 'default',
  effort: 'high',
  input_size: 1,
  thinking_enabled: false,
  workload: 'text',
  score_kind: 'text_stream',
  score_window_minutes: 1440,
  known: true,
  reliability: 75,
  latency: 80,
  sample_count: 4,
  latest_sample_at: observedAt,
  health: null,
}

function member(id: number, name: string): AdaptiveScoreMember {
  return {
    id, name, platform: 'openai', type: 'apikey', status: 'active',
    schedulable: true, schedulable_now: true, auto_pause_on_expired: false, expires_at: null,
    rate_limit_reset_at: null, overload_until: null, temp_unschedulable_until: null,
    overall_score: null, overall_sample_count: 0,
    v1: [], v2: [], v1_truncated: false, v2_truncated: false,
  }
}

function page(groupId: number, items: AdaptiveScoreMember[], pageNumber = 1): AdaptiveAccountScoresPage {
  return {
    group_id: groupId, items, page: pageNumber, page_size: 20, total: 40, max_queryable_total: 40,
    observed_at: observedAt, scope: 'current_instance', v2_ready: false,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => { resolve = done })
  return { promise, resolve }
}

let wrapper: VueWrapper | undefined

function setup(pinia = createPinia()) {
  const i18n = createI18n({
    legacy: false, locale: 'en', messages: {
      en: {
        admin: { ...accountScores, groups: { searchGroups: 'Search groups' } },
        common: { loading: 'Loading', refresh: 'Refresh' },
        pagination: { next: 'Next', previous: 'Previous', pageOf: '{page} of {total}' },
      },
    },
  })
  wrapper = mount(AccountScoresView, {
    global: {
      plugins: [pinia, i18n],
      stubs: {
        AppLayout: { template: '<main><slot /></main>' },
        TablePageLayout: { template: '<div data-test="table-page"><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        BaseDialog: {
          props: ['show'],
          emits: ['close'],
          template: '<div v-if="show" role="dialog"><button @click="$emit(\'close\')">Close dialog</button><slot /></div>',
        },
        Select: {
          props: { options: Array, remote: Boolean, modelValue: [String, Number] },
          emits: ['change', 'search'],
          template: '<div><button v-for="item in options" :key="item.value" :aria-pressed="modelValue === item.value" @click="$emit(\'change\', item.value, item)">{{ item.label }}</button><button v-if="remote" data-test="search" @click="$emit(\'search\', \'query\')">Search</button></div>',
        },
        DataTable: {
          props: ['data', 'columns'],
          template: '<div><div v-for="row in data" :key="row.id" :data-test="\'row-\' + row.id"><div v-for="column in columns" :key="column.key" :data-test="column.key"><slot :name="\'cell-\' + column.key" :row="row" /></div></div></div>',
        },
        Pagination: {
          props: ['page', 'pageSize', 'total'],
          emits: ['update:page'],
          template: '<div><button data-test="member-next" @click="$emit(\'update:page\', page + 1)">Member next</button><button data-test="member-last" @click="$emit(\'update:page\', Math.ceil(total / pageSize))">Member last</button></div>',
        },
      },
    },
  })
  return { wrapper, store: useAdminSettingsStore(pinia) }
}

function button(view: VueWrapper, text: string) {
  const match = view.findAll('button').find((item) => item.text() === text)
  if (!match) throw new Error(`Button missing: ${text}`)
  return match
}

beforeEach(() => {
  vi.resetAllMocks()
  localStorage.clear()
  api.settings.mockResolvedValue({ gateway_adaptive_scheduling_enabled: true })
  api.payment.mockResolvedValue({ data: {} })
  api.groups.mockResolvedValue({
    page: 1, pages: 2, total: 22, page_size: 20, items: [{ id: 1, name: 'Group A' }, { id: 2, name: 'Group B' }],
  })
  api.scores.mockResolvedValue(page(1, [member(1, 'A first')]))
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.restoreAllMocks()
  vi.useRealTimers()
})

describe('Account Scores', () => {
  it('waits for the admin feature gate before querying groups or members', async () => {
    const confirmation = deferred<{ gateway_adaptive_scheduling_enabled: boolean }>()
    api.settings.mockReturnValueOnce(confirmation.promise)
    const { wrapper } = setup()
    expect(wrapper.text()).toContain('Loading')
    expect(api.groups).not.toHaveBeenCalled()
    confirmation.resolve({ gateway_adaptive_scheduling_enabled: false })
    await flushPromises()
    expect(wrapper.text()).toContain('Adaptive scheduling is off.')
    expect(api.groups).not.toHaveBeenCalled()
    expect(api.scores).not.toHaveBeenCalled()
  })

  it('paginates group search and aborts stale member requests on page and group changes', async () => {
    const stale = deferred<AdaptiveAccountScoresPage>()
    api.scores.mockResolvedValueOnce(page(1, [member(1, 'A first')]))
      .mockReturnValueOnce(stale.promise)
      .mockResolvedValueOnce(page(2, [member(3, 'B first')]))
    const { wrapper } = setup()
    await flushPromises()
    expect(api.scores).not.toHaveBeenCalled()
    await button(wrapper, 'Next').trigger('click')
    await flushPromises()
    expect(api.groups).toHaveBeenLastCalledWith(2, 20, { search: undefined }, { signal: expect.any(AbortSignal) })
    await wrapper.get('[data-test="search"]').trigger('click')
    await flushPromises()
    expect(api.groups).toHaveBeenLastCalledWith(1, 20, { search: 'query' }, { signal: expect.any(AbortSignal) })
    await button(wrapper, 'Group A').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="member-next"]').trigger('click')
    expect(api.scores).toHaveBeenLastCalledWith(1, 2, 20, expect.any(AbortSignal))
    const signal = api.scores.mock.calls[1][3] as AbortSignal
    await button(wrapper, 'Group B').trigger('click')
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(api.scores).toHaveBeenLastCalledWith(2, 1, 20, expect.any(AbortSignal))
    stale.resolve(page(1, [member(2, 'Stale A page')], 2))
    await flushPromises()
    expect(wrapper.text()).toContain('B first')
    expect(wrapper.text()).not.toContain('Stale A page')
  })

  it('compares a single complete dimension and switches to the actual v2 states', async () => {
    const scored = member(1, 'Account with evidence')
    scored.overall_score = 72
    scored.overall_sample_count = 12
    scored.v1 = [{ ...bucket, model: 'older-model', latest_sample_at: '2026-11-26T07:00:00Z' }, bucket]
    scored.v2 = [{
      ...bucket, model: 'chat-model', wire: 'chat_completions', workloads: ['text'],
      state: 'slow', state_count: 2, latest_first_output_ms: 31000, latest_sample_at: '2026-11-26T08:04:00Z',
    }]
    const insufficient = { ...member(2, 'Insufficient account'), overall_sample_count: 1, v1: [
      { ...bucket, known: false, reliability: null, latency: null, sample_count: 1 },
    ] }
    const mismatch = { ...member(3, 'Different request bucket'), v1: [
      { ...bucket, input_size: 2, latency: 100, latest_sample_at: '2026-11-26T08:02:00Z' },
    ] }
    const match = { ...member(4, 'Same request bucket'), overall_score: 0, overall_sample_count: 4,
      v1: [{ ...bucket, latency: 40, reliability: 50 }] }
    api.scores.mockResolvedValue(page(1, [scored, insufficient, mismatch, match, member(5, 'No samples')]))
    const { wrapper } = setup()
    await flushPromises()
    await button(wrapper, 'Group A').trigger('click')
    await flushPromises()
    expect(wrapper.get('#account-scores-dimension button[aria-pressed="true"]').text()).toContain('recent-sol')
    const value = (id: number, column = 'metric') => wrapper.get(`[data-test="row-${id}"] [data-test="${column}"]`).text()
    expect(value(1)).toBe('80')
    expect(value(1, 'secondary')).toBe('75%')
    expect(value(2)).toBe('Insufficient samples')
    expect(value(2, 'samples')).toBe('1')
    expect(value(3)).toBe('—')
    expect(value(4)).toBe('40')
    expect(value(5)).toBe('—')
    expect(value(1, 'overall')).toMatch(/^72\s*Samples 12$/)
    expect(value(2, 'overall')).toMatch(/^—\s*Insufficient samples \(1\)$/)
    expect(value(4, 'overall')).toMatch(/^0\s*Samples 4$/)
    const v2 = wrapper.findAll('button').find((item) => item.text().startsWith('v2 / chat-model'))
    await v2?.trigger('click')
    expect(value(1)).toBe('Slow')
    expect(value(4)).toBe('—')
    expect(value(1, 'overall')).toMatch(/^72\s*Samples 12$/)
    expect(value(4, 'overall')).toMatch(/^0\s*Samples 4$/)
    expect(wrapper.find('[data-test="row-1"] [data-test="secondary"]').exists()).toBe(false)
    await button(wrapper, 'Show samples').trigger('click')
    expect(wrapper.get('[role="dialog"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="table-page"]').find('#account-score-details').exists()).toBe(false)
    expect(wrapper.get('#account-score-details').text()).toContain('older-model')
    expect(wrapper.get('#account-score-details').text()).toContain('v2 history is still loading.')
    await button(wrapper, 'Close dialog').trigger('click')
    expect(wrapper.find('#account-score-details').exists()).toBe(false)
  })

  it('keeps the member list when the group has no samples', async () => {
    api.scores.mockResolvedValueOnce(page(1, [member(1, 'No evidence yet')]))
    const { wrapper } = setup()
    await flushPromises()
    await button(wrapper, 'Group A').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('No samples in the current window.')
    expect(wrapper.text()).toContain('No evidence yet')
    expect(wrapper.get('[data-test="row-1"] [data-test="metric"]').text()).toBe('—')
    expect(wrapper.get('[data-test="row-1"] [data-test="overall"]').text()).toMatch(/^—\s*Samples 0$/)
    expect(wrapper.get('[data-test="row-1"] [data-test="samples"]').text()).toBe('—')
  })

  it('shows an actual zero reliability band and leaves missing latency blank', async () => {
    const scored = { ...member(1, 'General bucket'), v1: [
      { ...bucket, score_kind: 'general' as const, reliability: 0, latency: null, transport: 'http' },
    ] }
    api.scores.mockResolvedValueOnce(page(1, [scored]))
    const { wrapper } = setup()
    await flushPromises()
    await button(wrapper, 'Group A').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-test="row-1"] [data-test="metric"]').text()).toBe('0/5')
    expect(wrapper.get('[data-test="row-1"] [data-test="secondary"]').text()).toBe('—')
  })

  it('pauses hidden polling, retains data on transient failure, then stops when the gate closes', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] })
    const visible = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    api.scores.mockResolvedValueOnce(page(1, [member(1, 'Retained account')]))
      .mockRejectedValueOnce({ status: 503 })
      .mockRejectedValueOnce({ status: 404, reason: 'ADAPTIVE_ACCOUNT_SCORES_DISABLED' })
    const { wrapper, store } = setup()
    await flushPromises()
    await button(wrapper, 'Group A').trigger('click')
    await flushPromises()
    visible.mockReturnValue(true)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(api.scores).toHaveBeenCalledTimes(1)
    visible.mockReturnValue(false)
    await vi.advanceTimersByTimeAsync(15_000)
    expect(api.scores).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('Could not refresh this page.')
    expect(wrapper.text()).toContain('Retained account')
    await vi.advanceTimersByTimeAsync(29_000)
    expect(api.scores).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(1000)
    expect(api.scores).toHaveBeenCalledTimes(3)
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(false)
    expect(wrapper.text()).not.toContain('Retained account')
    expect(wrapper.text()).toContain('Adaptive scheduling is off.')
    await vi.advanceTimersByTimeAsync(60_000)
    expect(api.scores).toHaveBeenCalledTimes(3)
  })

  it('keeps a disabling 404 ahead of an older confirmation and rechecks on re-entry', async () => {
    const pinia = createPinia()
    const store = useAdminSettingsStore(pinia)
    await store.fetch()
    expect(store.loaded).toBe(true)
    const first = setup(pinia).wrapper
    await flushPromises()
    const older = deferred<{ gateway_adaptive_scheduling_enabled: boolean }>()
    api.settings.mockReturnValueOnce(older.promise)
    await button(first, 'Refresh').trigger('click')
    api.scores.mockRejectedValueOnce({ status: 404, reason: 'ADAPTIVE_ACCOUNT_SCORES_DISABLED' })
    await button(first, 'Group A').trigger('click')
    await flushPromises()
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(false)
    older.resolve({ gateway_adaptive_scheduling_enabled: true })
    await flushPromises()
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(false)
    expect(first.text()).toContain('Adaptive scheduling is off.')
    first.unmount()

    const second = setup(pinia).wrapper
    await flushPromises()
    expect(api.settings).toHaveBeenCalledTimes(4)
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(true)
    expect(second.text()).toContain('Group A')
    expect(api.scores).toHaveBeenCalledTimes(1)
  })

  it('can recheck with no selected group and recover from the disabled view', async () => {
    const { wrapper } = setup()
    await flushPromises()
    api.settings.mockResolvedValueOnce({ gateway_adaptive_scheduling_enabled: false })
    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Adaptive scheduling is off.')
    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Group A')
    expect(wrapper.text()).not.toContain('Adaptive scheduling is off.')
    expect(api.scores).not.toHaveBeenCalled()
  })

  it('uses the returned member range and never requests page 502', async () => {
    const firstPage = { ...page(1, [member(1, 'First')]), total: 10021, max_queryable_total: 10020 }
    api.scores.mockResolvedValueOnce(firstPage)
      .mockResolvedValueOnce({ ...firstPage, page: 501, items: [member(10020, 'Last accessible')] })
    const { wrapper } = setup()
    await flushPromises()
    await button(wrapper, 'Group A').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Showing up to 10020 of 10021 members.')
    await wrapper.get('[data-test="member-last"]').trigger('click')
    await flushPromises()
    expect(api.scores).toHaveBeenLastCalledWith(1, 501, 20, expect.any(AbortSignal))
    await wrapper.get('[data-test="member-next"]').trigger('click')
    await flushPromises()
    expect(api.scores).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('Last accessible')
  })
})
