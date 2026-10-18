import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { UserMonitorDetail, UserMonitorView } from '@/api/channelMonitor'
import ChannelStatusV1View from '../ChannelStatusV1View.vue'

const mocks = vi.hoisted(() => ({ list: vi.fn(), status: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/channelMonitor', () => mocks)
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    cachedPublicSettings: { channel_monitor_enabled: true },
    showError: mocks.showError,
  }),
}))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key }),
}))

const cardStub = {
  props: ['item', 'window', 'availabilityValue', 'countdownSeconds'],
  emits: ['click'],
  template: `<button
    class="monitor-card"
    :data-id="item.id"
    :data-window="window"
    :data-value="availabilityValue === null ? 'empty' : availabilityValue"
    @click="$emit('click')"
  >{{ item.name }}</button>`,
}

enableAutoUnmount(afterEach)

function monitor(id: number, name: string, availability: number): UserMonitorView {
  return {
    id,
    name,
    provider: 'openai',
    group_name: '',
    primary_model: 'model',
    primary_status: 'operational',
    primary_latency_ms: null,
    primary_ping_latency_ms: null,
    availability_7d: availability,
    extra_models: [],
    timeline: [],
  }
}

function detail(id: number, availability15: number, availability30: number): UserMonitorDetail {
  return {
    id,
    name: `Monitor ${id}`,
    provider: 'openai',
    group_name: '',
    models: [{
      model: 'model',
      latest_status: 'operational',
      latest_latency_ms: null,
      availability_7d: 0,
      availability_15d: availability15,
      availability_30d: availability30,
      avg_latency_7d_ms: null,
    }],
  }
}

function mountPage() {
  return mount(ChannelStatusV1View, {
    global: {
      stubs: {
        AppLayout: { template: '<main><slot /></main>' },
        MonitorHero: {
          props: ['window', 'autoRefresh', 'loading'],
          emits: ['update:window', 'refresh'],
          template: `<section>
            <button class="window-7d" @click="$emit('update:window', '7d')">7d</button>
            <button class="window-15d" @click="$emit('update:window', '15d')">15d</button>
            <button class="window-30d" @click="$emit('update:window', '30d')">30d</button>
            <button class="manual-refresh" @click="$emit('refresh')">Refresh</button>
            <button class="interval-30" @click="autoRefresh.setInterval(30)">30 seconds</button>
          </section>`,
        },
        MonitorCard: cardStub,
        MonitorDetailDialog: {
          props: ['show', 'monitorId', 'title'],
          emits: ['close'],
          template: `<div v-if="show" class="detail-dialog" :data-id="monitorId" :data-title="title">
            <button class="detail-close" @click="$emit('close')">Close</button>
          </div>`,
        },
        EmptyState: { template: '<div class="empty-state"></div>' },
      },
    },
  })
}

function cardIds(wrapper: ReturnType<typeof mountPage>): number[] {
  return wrapper.findAll('.monitor-card').map(card => Number(card.attributes('data-id')))
}

function cardValues(wrapper: ReturnType<typeof mountPage>): string[] {
  return wrapper.findAll('.monitor-card').map(card => card.attributes('data-value') ?? 'empty')
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.resetAllMocks()
  localStorage.clear()
  mocks.list.mockResolvedValue({ items: [] })
  mocks.status.mockResolvedValue(detail(1, 50, 50))
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.resetAllMocks()
  vi.useRealTimers()
  localStorage.clear()
})

describe('ChannelStatusV1View sorted monitor grid', () => {
  it('uses list availability for initial, manual and automatic 7d refreshes without detail requests', async () => {
    const [a, b, c] = [monitor(1, 'A', 90), monitor(2, 'B', 80), monitor(3, 'C', 70)]
    mocks.list
      .mockResolvedValueOnce({ items: [a, b, c] })
      .mockResolvedValueOnce({ items: [monitor(1, 'A', 50), monitor(2, 'B', 99), monitor(3, 'C', 60)] })
      .mockResolvedValueOnce({ items: [monitor(1, 'A', 100), monitor(2, 'B', 0), monitor(3, 'C', 30)] })
    const wrapper = mountPage()
    await flushPromises()

    expect(cardIds(wrapper)).toEqual([1, 2, 3])
    expect(cardValues(wrapper)).toEqual(['90', '80', '70'])
    await wrapper.get('.manual-refresh').trigger('click')
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([2, 3, 1])
    expect(cardValues(wrapper)).toEqual(['99', '60', '50'])

    await wrapper.get('.interval-30').trigger('click')
    await vi.advanceTimersByTimeAsync(30000)
    await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(3)
    expect(mocks.status).not.toHaveBeenCalled()
    expect(cardIds(wrapper)).toEqual([1, 3, 2])
    expect(cardValues(wrapper)).toEqual(['100', '30', '0'])
  })

  it('uses each selected window, then restores the latest 7d list order', async () => {
    const [a, b, c] = [monitor(1, 'A', 90), monitor(2, 'B', 80), monitor(3, 'C', 70)]
    mocks.list.mockResolvedValueOnce({ items: [c, a, b] })
    const byID: Record<number, UserMonitorDetail> = {
      1: detail(1, 20, 40),
      2: detail(2, 100, 10),
      3: detail(3, 50, 95),
    }
    mocks.status.mockImplementation(async (id: number) => byID[id])
    const wrapper = mountPage()
    await flushPromises()

    expect(cardIds(wrapper)).toEqual([a.id, b.id, c.id])
    await wrapper.get('.window-15d').trigger('click')
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([b.id, c.id, a.id])
    expect(cardValues(wrapper)).toEqual(['100', '50', '20'])
    await wrapper.get('.window-30d').trigger('click')
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([c.id, a.id, b.id])
    expect(cardValues(wrapper)).toEqual(['95', '40', '10'])
    await wrapper.get('.window-7d').trigger('click')
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([a.id, b.id, c.id])
    expect(cardValues(wrapper)).toEqual(['90', '80', '70'])
    expect(mocks.status).toHaveBeenCalledTimes(3)
  })

  it('keeps cached values on a failed detail refresh and reorders to successfully refreshed values', async () => {
    const [a, b, c] = [monitor(1, 'A', 90), monitor(2, 'B', 80), monitor(3, 'C', 70)]
    mocks.list
      .mockResolvedValueOnce({ items: [a, b, c] })
      .mockResolvedValueOnce({ items: [a, b, c] })
      .mockResolvedValueOnce({ items: [a, b, c] })
    const initial: Record<number, UserMonitorDetail> = {
      1: detail(1, 90, 30),
      2: detail(2, 80, 20),
      3: detail(3, 70, 10),
    }
    mocks.status.mockImplementation(async (id: number) => {
      if (id === 3) throw new Error('initial detail request failed')
      return initial[id]
    })
    const wrapper = mountPage()
    await flushPromises()
    await wrapper.get('.window-15d').trigger('click')
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([1, 2, 3])
    expect(cardValues(wrapper)).toEqual(['90', '80', 'empty'])
    expect(mocks.showError).toHaveBeenCalledTimes(1)

    mocks.status.mockImplementation(async (id: number) => {
      if (id === 2) throw new Error('detail refresh failed')
      return id === 1 ? detail(1, 60, 30) : detail(3, 100, 10)
    })
    await wrapper.get('.manual-refresh').trigger('click')
    await flushPromises()

    expect(mocks.showError).toHaveBeenCalledTimes(2)
    expect(cardIds(wrapper)).toEqual([3, 2, 1])
    expect(cardValues(wrapper)).toEqual(['100', '80', '60'])
    expect(wrapper.findAll('.monitor-card').every(card => card.attributes('data-window') === '15d')).toBe(true)

    mocks.status.mockImplementation(async (id: number) => ({
      1: detail(1, 95, 30),
      2: detail(2, 80, 20),
      3: detail(3, 60, 10),
    })[id])
    await wrapper.get('.interval-30').trigger('click')
    await vi.advanceTimersByTimeAsync(30000)
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([1, 2, 3])
    expect(cardValues(wrapper)).toEqual(['95', '80', '60'])
    expect(wrapper.findAll('.monitor-card').every(card => card.attributes('data-window') === '15d')).toBe(true)
  })

  it('keeps the active window while detail requests resolve out of order after rapid switching', async () => {
    const [a, b, c] = [monitor(1, 'A', 90), monitor(2, 'B', 80), monitor(3, 'C', 70)]
    mocks.list.mockResolvedValueOnce({ items: [a, b, c] })
    const requests = new Map<string, ReturnType<typeof deferred<UserMonitorDetail>>>()
    for (const id of [1, 2, 3]) {
      requests.set(`${id}:1`, deferred<UserMonitorDetail>())
      requests.set(`${id}:2`, deferred<UserMonitorDetail>())
    }
    const counts = new Map<number, number>()
    mocks.status.mockImplementation((id: number) => {
      const count = (counts.get(id) ?? 0) + 1
      counts.set(id, count)
      return requests.get(`${id}:${count}`)!.promise
    })
    const wrapper = mountPage()
    await flushPromises()

    await wrapper.get('.window-15d').trigger('click')
    expect(mocks.status).toHaveBeenCalledTimes(3)
    await wrapper.get('.window-30d').trigger('click')
    expect(mocks.status).toHaveBeenCalledTimes(6)

    requests.get('3:1')!.resolve(detail(3, 50, 95))
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([3, 1, 2])
    expect(cardValues(wrapper)).toEqual(['95', 'empty', 'empty'])
    requests.get('2:1')!.resolve(detail(2, 100, 10))
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([3, 2, 1])
    requests.get('1:1')!.resolve(detail(1, 20, 40))
    await flushPromises()
    expect(cardIds(wrapper)).toEqual([3, 1, 2])
    expect(cardValues(wrapper)).toEqual(['95', '40', '10'])
    expect(wrapper.findAll('.monitor-card').every(card => card.attributes('data-window') === '30d')).toBe(true)

    const refreshedDetails: Record<number, UserMonitorDetail> = {
      1: detail(1, 20, 40),
      2: detail(2, 100, 10),
      3: detail(3, 50, 95),
    }
    for (const id of [1, 2, 3]) requests.get(`${id}:2`)!.resolve(refreshedDetails[id])
    await flushPromises()
    await wrapper.get('.window-15d').trigger('click')
    expect(cardIds(wrapper)).toEqual([2, 3, 1])
    await wrapper.get('.window-7d').trigger('click')
    expect(cardIds(wrapper)).toEqual([1, 2, 3])
  })

  it.each([
    { window: '7d' as const, expectedID: 2 },
    { window: '15d' as const, expectedID: 1 },
    { window: '30d' as const, expectedID: 3 },
  ])('opens and closes the reordered $window card with its own ID and title', async ({ window, expectedID }) => {
    const items = [monitor(1, 'Alpha', 80), monitor(2, 'Beta', 95), monitor(3, 'Gamma', 70)]
    mocks.list.mockResolvedValueOnce({ items })
    mocks.status.mockImplementation(async (id: number) => ({
      1: detail(1, 90, 20),
      2: detail(2, 50, 30),
      3: detail(3, 70, 100),
    })[id])
    const wrapper = mountPage()
    await flushPromises()
    if (window !== '7d') {
      await wrapper.get(`.window-${window}`).trigger('click')
      await flushPromises()
    }
    expect(cardIds(wrapper)[0]).toBe(expectedID)
    await wrapper.get('.monitor-card').trigger('click')
    expect(wrapper.get('.detail-dialog').attributes('data-id')).toBe(String(expectedID))
    expect(wrapper.get('.detail-dialog').attributes('data-title')).toBe(items.find(item => item.id === expectedID)!.name)
    await wrapper.get('.detail-close').trigger('click')
    expect(wrapper.find('.detail-dialog').exists()).toBe(false)
  })
})
