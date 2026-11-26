import { mount, flushPromises, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AdaptiveAccountScoresPage, AdaptiveScoreMember, AdaptiveScoreOption, AdaptiveScoreV1 } from '@/api/admin/groups'
import accountScores from '@/i18n/locales/en/admin/accountScores'
import { useAdminSettingsStore } from '@/stores/adminSettings'
import AccountScoresView from '../AccountScoresView.vue'
import EmptyState from '@/components/common/EmptyState.vue'

const api = vi.hoisted(() => ({
  groups: vi.fn(), scores: vi.fn(), settings: vi.fn(), payment: vi.fn(),
}))

vi.mock('@/api', () => ({
  adminAPI: {
    groups: { listAdaptiveScoreOptions: api.groups, getAdaptiveAccountScores: api.scores },
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

function groupPage(items: { id: number; name: string; description?: string }[], nextAfterId: number | null = null) {
  return { items: items.map(({ description = '', ...item }): AdaptiveScoreOption => ({ ...item, description })), next_after_id: nextAfterId }
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
        common: { loading: 'Loading', refresh: 'Refresh', noData: 'No data', noOptionsFound: 'No options' },
        pagination: { next: 'Next', previous: 'Previous', pageOf: '{page} of {total}' },
      },
    },
  })
  wrapper = mount(AccountScoresView, {
    attachTo: document.body,
    global: {
      plugins: [pinia, i18n],
      stubs: {
        teleport: true,
        AppLayout: { template: '<main><slot /></main>' },
        DataTable: {
          props: ['data', 'columns'],
          template: `
            <div>
              <slot v-if="data.length === 0" name="empty" />
              <div v-for="row in data" :key="row.id" :data-test="'row-' + row.id">
                <div v-for="column in columns" :key="column.key" :data-test="column.key">
                  <slot :name="'cell-' + column.key" :row="row" />
                </div>
              </div>
            </div>
          `,
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

function groupOptions(view: VueWrapper) {
  return view.findAll('[role="listbox"] [role="option"]')
}

async function selectGroup(view: VueWrapper, name: string) {
  await view.get('#account-scores-group').trigger('click')
  const option = groupOptions(view).find((item) => item.text() === name)
  if (!option) throw new Error(`Group missing: ${name}`)
  await option.trigger('click')
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
  api.groups.mockResolvedValue(groupPage([{ id: 1, name: 'Group A' }, { id: 2, name: 'Group B' }]))
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


  it('shows a compact prompt and searches both pages of groups locally in order', async () => {
    const groups = Array.from({ length: 31 }, (_, i) => ({ id: i + 1, name: `Group ${i + 1}` }))
    api.groups.mockResolvedValueOnce(groupPage(groups, 31))
      .mockResolvedValueOnce(groupPage([
        { id: 32, name: 'Disabled group' },
        { id: 33, name: 'Composite group' },
        { id: 34, name: 'Unsampled group', description: 'Only match by description' },
      ]))
    api.scores.mockResolvedValueOnce({ ...page(34, []), total: 0, max_queryable_total: 0 })
    const { wrapper } = setup()
    await flushPromises()

    const empty = wrapper.getComponent(EmptyState)
    const trigger = wrapper.get('#account-scores-group')
    expect(empty.text()).toContain('Select a group')
    expect(empty.element.parentElement?.classList.contains('card')).toBe(true)
    expect(empty.element.parentElement?.contains(trigger.element)).toBe(true)
    expect(wrapper.find('.table-page-layout').exists()).toBe(false)
    expect(wrapper.findAll('button')).toHaveLength(1)
    expect(wrapper.find('[data-test="member-next"]').exists()).toBe(false)
    expect(api.scores).not.toHaveBeenCalled()
    expect(api.groups).toHaveBeenCalledTimes(2)
    const signal = api.groups.mock.calls[0][1] as AbortSignal
    expect(signal).toBeInstanceOf(AbortSignal)
    expect(api.groups.mock.calls).toEqual([[0, signal], [31, signal]])

    await trigger.trigger('click')
    expect(groupOptions(wrapper)).toHaveLength(34)
    expect(groupOptions(wrapper)[30].text()).toBe('Group 31')
    expect(groupOptions(wrapper).slice(-3).map((option) => option.text())).toEqual([
      'Disabled group', 'Composite group', 'Unsampled group',
    ])
    const search = wrapper.get('[aria-label="Search groups"]')
    await search.setValue('match by description')
    expect(groupOptions(wrapper).map((option) => option.text())).toEqual(['Unsampled group'])
    await groupOptions(wrapper)[0].trigger('click')
    await flushPromises()
    expect(api.groups).toHaveBeenCalledTimes(2)
    expect(api.scores).toHaveBeenCalledTimes(1)
    expect(api.scores).toHaveBeenCalledWith(34, 1, 20, expect.any(AbortSignal))
    expect(wrapper.get('#account-scores-group').element).toBe(trigger.element)
    expect(document.activeElement).toBe(trigger.element)
    expect(wrapper.findComponent(EmptyState).exists()).toBe(false)
    expect(wrapper.text()).toContain('This group has no accounts.')
    await wrapper.get('[aria-label="Clear selection"]').trigger('click')
    expect(wrapper.findComponent(EmptyState).exists()).toBe(true)
    expect(api.scores).toHaveBeenCalledTimes(1)
  })

  it('stops on a non-advancing cursor and allows retry from the beginning', async () => {
    api.groups.mockResolvedValueOnce(groupPage([{ id: 3, name: 'Incomplete listing' }], 3))
      .mockResolvedValueOnce(groupPage([{ id: 4, name: 'Malformed continuation' }], 3))
    const { wrapper } = setup()
    await flushPromises()
    expect(api.groups.mock.calls.map(([afterId]) => afterId)).toEqual([0, 3])
    expect(wrapper.text()).toContain('Could not load groups.')
    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    expect(api.groups.mock.calls.map(([afterId]) => afterId)).toEqual([0, 3, 0])
    await selectGroup(wrapper, 'Group A')
    expect(api.scores).toHaveBeenCalledWith(1, 1, 20, expect.any(AbortSignal))
  })

  it('removes a deleted group on a score 404 and retries a failed directory refresh', async () => {
    const { wrapper, store } = setup()
    await flushPromises()
    await selectGroup(wrapper, 'Group A')
    await flushPromises()
    expect(wrapper.text()).toContain('A first')

    api.scores.mockRejectedValueOnce({ status: 404, reason: 'GROUP_NOT_FOUND' })
    api.groups.mockRejectedValueOnce({ status: 503 })
      .mockResolvedValueOnce(groupPage([{ id: 2, name: 'Group B' }, { id: 3, name: 'New group' }]))
    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(true)
    expect(wrapper.text()).toContain('This group is no longer available.')
    expect(wrapper.text()).toContain('Could not load groups.')
    expect(wrapper.text()).not.toContain('A first')
    const trigger = wrapper.get('#account-scores-group')
    await trigger.trigger('click')
    expect(groupOptions(wrapper).map((option) => option.text())).toEqual(['Group B'])
    await trigger.trigger('click')

    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    expect(api.groups.mock.calls.map(([afterId]) => afterId)).toEqual([0, 0, 0])
    expect(wrapper.text()).toContain('This group is no longer available.')
    expect(wrapper.text()).not.toContain('Could not load groups.')
    await selectGroup(wrapper, 'New group')
    expect(api.scores).toHaveBeenLastCalledWith(3, 1, 20, expect.any(AbortSignal))
    expect(wrapper.text()).not.toContain('This group is no longer available.')
  })

  it('aborts stale member requests on page and group changes', async () => {
    const stale = deferred<AdaptiveAccountScoresPage>()
    api.scores.mockResolvedValueOnce(page(1, [member(1, 'A first')]))
      .mockReturnValueOnce(stale.promise)
      .mockResolvedValueOnce(page(2, [member(3, 'B first')]))
    const { wrapper } = setup()
    await flushPromises()
    expect(api.scores).not.toHaveBeenCalled()
    await selectGroup(wrapper, 'Group A')
    await flushPromises()
    await wrapper.get('[data-test="member-next"]').trigger('click')
    expect(api.scores).toHaveBeenLastCalledWith(1, 2, 20, expect.any(AbortSignal))
    const signal = api.scores.mock.calls[1][3] as AbortSignal
    await selectGroup(wrapper, 'Group B')
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(api.scores).toHaveBeenLastCalledWith(2, 1, 20, expect.any(AbortSignal))
    stale.resolve(page(1, [member(2, 'Stale A page')], 2))
    await flushPromises()
    expect(wrapper.text()).toContain('B first')
    expect(wrapper.text()).not.toContain('Stale A page')
  })


  it('shows only the overall score, sample count and scheduling status beside the account', async () => {
    const scored: AdaptiveScoreMember = {
      ...member(1, 'Account with evidence'),
      overall_score: 72,
      overall_sample_count: 12,
      v1: [bucket],
      v2: [{
        ...bucket, model: 'chat-model', wire: 'chat_completions', workloads: ['text'],
        state: 'slow', state_count: 2, latest_first_output_ms: 31000,
      }],
    }
    const insufficient = { ...member(2, 'Insufficient account'), overall_sample_count: 1 }
    const zero = { ...member(3, 'Zero score'), overall_score: 0, overall_sample_count: 4, schedulable_now: false }
    api.scores.mockResolvedValueOnce(page(1, [scored, insufficient, zero, member(4, 'No samples')]))
    const { wrapper } = setup()
    await flushPromises()
    await selectGroup(wrapper, 'Group A')
    await flushPromises()
    const cells = (id: number) => wrapper.get(`[data-test="row-${id}"]`)
    const overall = (id: number) => cells(id).get('[data-test="overall"]').text()
    expect(cells(1).findAll('[data-test]').map((cell) => cell.attributes('data-test'))).toEqual(['name', 'overall', 'status'])
    expect(cells(1).get('[data-test="name"]').text()).toContain('Account with evidence')
    expect(cells(1).get('[data-test="status"]').text()).toBe('Schedulable')
    expect(cells(3).get('[data-test="status"]').text()).toBe('Unavailable')
    expect(overall(1)).toMatch(/^72\s*Samples 12$/)
    expect(overall(2)).toMatch(/^—\s*Insufficient samples \(1\)$/)
    expect(overall(3)).toMatch(/^0\s*Samples 4$/)
    expect(overall(4)).toMatch(/^—\s*Samples 0$/)
    expect(wrapper.find('#account-scores-dimension').exists()).toBe(false)
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(wrapper.text()).not.toMatch(/Show samples|recent-sol|chat-model/)

    api.scores.mockResolvedValueOnce(page(1, [{ ...scored, overall_score: 81 }]))
    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    expect(api.scores).toHaveBeenLastCalledWith(1, 1, 20, expect.any(AbortSignal))
    expect(overall(1)).toMatch(/^81\s*Samples 12$/)
  })

  it('keeps the member list when the entire group has no samples', async () => {
    api.scores.mockResolvedValueOnce(page(1, [member(1, 'No evidence yet')]))
    const { wrapper } = setup()
    await flushPromises()
    await selectGroup(wrapper, 'Group A')
    await flushPromises()
    expect(wrapper.text()).toContain('No evidence yet')
    expect(wrapper.get('[data-test="row-1"] [data-test="overall"]').text()).toMatch(/^—\s*Samples 0$/)
  })

  it('pauses hidden polling, retains data on transient failure, then stops when the gate closes', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'] })
    const visible = vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    api.scores.mockResolvedValueOnce(page(1, [member(1, 'Retained account')]))
      .mockRejectedValueOnce({ status: 503 })
      .mockRejectedValueOnce({ status: 404, reason: 'ADAPTIVE_ACCOUNT_SCORES_DISABLED' })
    const { wrapper, store } = setup()
    await flushPromises()
    await selectGroup(wrapper, 'Group A')
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
    const olderConfirmation = store.refreshGatewayAdaptiveScheduling()
    api.scores.mockRejectedValueOnce({ status: 404, reason: 'ADAPTIVE_ACCOUNT_SCORES_DISABLED' })
    await selectGroup(first, 'Group A')
    await flushPromises()
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(false)
    older.resolve({ gateway_adaptive_scheduling_enabled: true })
    await olderConfirmation
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(false)
    expect(first.text()).toContain('Adaptive scheduling is off.')
    first.unmount()

    const second = setup(pinia).wrapper
    await flushPromises()
    expect(api.settings).toHaveBeenCalledTimes(4)
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(true)
    expect(second.get('#account-scores-group').exists()).toBe(true)
    expect(api.scores).toHaveBeenCalledTimes(1)
  })


  it('discards an aborted group list across disable and re-enable and cancels pending scores on unmount', async () => {
    const oldGroups = deferred<ReturnType<typeof groupPage>>()
    const pendingScores = deferred<AdaptiveAccountScoresPage>()
    api.groups.mockResolvedValueOnce(groupPage([{ id: 3, name: 'Partial listing' }], 3))
      .mockReturnValueOnce(oldGroups.promise)
    api.scores.mockReturnValueOnce(pendingScores.promise)
    const { wrapper, store } = setup()
    await flushPromises()
    const groupSignal = api.groups.mock.calls[1][1] as AbortSignal

    store.setGatewayAdaptiveSchedulingEnabled(false)
    await flushPromises()
    expect(groupSignal.aborted).toBe(true)
    expect(wrapper.text()).toContain('Adaptive scheduling is off.')
    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    expect(api.groups).toHaveBeenCalledTimes(3)

    oldGroups.resolve(groupPage([{ id: 4, name: 'Stale group' }], 4))
    await flushPromises()
    expect(api.groups.mock.calls.map(([afterId]) => afterId)).toEqual([0, 3, 0])
    await selectGroup(wrapper, 'Group A')
    expect(api.scores).toHaveBeenCalledTimes(1)
    expect(api.scores).toHaveBeenCalledWith(1, 1, 20, expect.any(AbortSignal))
    const scoreSignal = api.scores.mock.calls[0][3] as AbortSignal
    wrapper.unmount()
    expect(scoreSignal.aborted).toBe(true)
    pendingScores.resolve(page(1, [member(1, 'Stale result')]))
    await flushPromises()
  })

  it('aborts scores on disable and requires a new selection after enabling', async () => {
    const oldScores = deferred<AdaptiveAccountScoresPage>()
    api.scores.mockReturnValueOnce(oldScores.promise)
    const { wrapper, store } = setup()
    await flushPromises()
    await selectGroup(wrapper, 'Group A')
    const signal = api.scores.mock.calls[0][3] as AbortSignal

    store.setGatewayAdaptiveSchedulingEnabled(false)
    await flushPromises()
    expect(signal.aborted).toBe(true)
    await button(wrapper, 'Refresh').trigger('click')
    await flushPromises()
    oldScores.resolve(page(1, [member(1, 'Stale result')]))
    await flushPromises()
    expect(wrapper.getComponent(EmptyState).exists()).toBe(true)
    expect(wrapper.text()).not.toContain('Stale result')
    expect(api.scores).toHaveBeenCalledTimes(1)
  })

  it('uses the returned member range and never requests page 502', async () => {
    const firstPage = { ...page(1, [member(1, 'First')]), total: 10021, max_queryable_total: 10020 }
    api.scores.mockResolvedValueOnce(firstPage)
      .mockResolvedValueOnce({ ...firstPage, page: 501, items: [member(10020, 'Last accessible')] })
    const { wrapper } = setup()
    await flushPromises()
    await selectGroup(wrapper, 'Group A')
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
