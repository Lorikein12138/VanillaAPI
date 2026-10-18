import { flushPromises, mount } from '@vue/test-utils'
import { reactive, nextTick } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { UserMonitorDetail, UserMonitorView } from '@/api/channelMonitor'
import MonitorCardGrid from '../monitor/MonitorCardGrid.vue'

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

function monitor(
  id: number,
  name: string,
  availability: number,
  status: UserMonitorView['primary_status'] = 'operational',
): UserMonitorView {
  return {
    id,
    name,
    provider: 'openai',
    group_name: '',
    primary_model: 'model',
    primary_status: status,
    primary_latency_ms: null,
    primary_ping_latency_ms: null,
    availability_7d: availability,
    extra_models: [],
    timeline: [],
  }
}

function detail(id: number, availability15: unknown, availability30: unknown): UserMonitorDetail {
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
      availability_15d: availability15 as number,
      availability_30d: availability30 as number,
      avg_latency_7d_ms: null,
    }],
  }
}

function mountGrid(
  items: UserMonitorView[],
  window: '7d' | '15d' | '30d' = '7d',
  detailCache: Record<number, UserMonitorDetail> = {},
  loading = false,
) {
  return mount(MonitorCardGrid, {
    props: { items, window, countdownSeconds: 0, loading, detailCache },
    global: {
      stubs: {
        MonitorCard: cardStub,
        EmptyState: { props: ['title', 'description'], template: '<div class="empty-state">{{ title }}</div>' },
      },
    },
  })
}

function cardIds(wrapper: ReturnType<typeof mountGrid>): number[] {
  return wrapper.findAll('.monitor-card').map(card => Number(card.attributes('data-id')))
}

function cardValues(wrapper: ReturnType<typeof mountGrid>): string[] {
  return wrapper.findAll('.monitor-card').map(card => card.attributes('data-value') ?? 'empty')
}

afterEach(() => vi.restoreAllMocks())

describe('MonitorCardGrid selected-window sorting', () => {
  it('sorts the eight supplied channels by their 7d value without status precedence', () => {
    const items = [
      monitor(1, 'DeepSeek', 90.56, 'failed'),
      monitor(2, 'Claude Max Ent', 97.78),
      monitor(3, 'Claude Max Sale', 93.33),
      monitor(4, 'ChatGPT Pro Sale', 81.11),
      monitor(5, 'ChatGPT Key', 90.06),
      monitor(6, 'ChatGPT Plus Sale', 79.44),
      monitor(7, 'ChatGPT Pro', 83.89),
      monitor(8, 'Gemini', 93.37),
    ]
    const wrapper = mountGrid(items)

    expect(wrapper.findAll('.monitor-card').map(card => card.text())).toEqual([
      'Claude Max Ent', 'Gemini', 'Claude Max Sale', 'DeepSeek',
      'ChatGPT Key', 'ChatGPT Pro', 'ChatGPT Pro Sale', 'ChatGPT Plus Sale',
    ])
    expect(cardValues(wrapper)).toEqual(['97.78', '93.37', '93.33', '90.56', '90.06', '83.89', '81.11', '79.44'])
    wrapper.unmount()
  })

  it('reorders by each selected window and displays the exact value used for ranking', async () => {
    const a = monitor(1, 'A', 90)
    const b = monitor(2, 'B', 80)
    const c = monitor(3, 'C', 70)
    const cache = reactive<Record<number, UserMonitorDetail>>({
      [a.id]: detail(a.id, 20, 40),
      [b.id]: detail(b.id, 100, 10),
      [c.id]: detail(c.id, 50, 95),
    })
    const wrapper = mountGrid([c, a, b], '7d', cache)

    expect(cardIds(wrapper)).toEqual([a.id, b.id, c.id])
    expect(cardValues(wrapper)).toEqual(['90', '80', '70'])
    await wrapper.setProps({ window: '15d' })
    expect(cardIds(wrapper)).toEqual([b.id, c.id, a.id])
    expect(cardValues(wrapper)).toEqual(['100', '50', '20'])
    await wrapper.setProps({ window: '30d' })
    expect(cardIds(wrapper)).toEqual([c.id, a.id, b.id])
    expect(cardValues(wrapper)).toEqual(['95', '40', '10'])
    await wrapper.setProps({ window: '7d' })
    expect(cardIds(wrapper)).toEqual([a.id, b.id, c.id])
    expect(cardValues(wrapper)).toEqual(['90', '80', '70'])
    wrapper.unmount()
  })

  it('keeps equal values stable and compares raw precision rather than rounded display text', () => {
    const firstEqual = monitor(1, 'First equal', 50)
    const lowerRaw = monitor(2, 'Lower raw', 90.561)
    const secondEqual = monitor(3, 'Second equal', 50)
    const higherRaw = monitor(4, 'Higher raw', 90.564)
    const wrapper = mountGrid([firstEqual, lowerRaw, secondEqual, higherRaw])

    expect(cardIds(wrapper)).toEqual([higherRaw.id, lowerRaw.id, firstEqual.id, secondEqual.id])
    expect(cardValues(wrapper)).toEqual(['90.564', '90.561', '50', '50'])
    wrapper.unmount()
  })

  it('keeps 0 and 100 valid and places invalid 7d values after valid values in source order', () => {
    const cases: Array<[string, unknown]> = [
      ['zero', 0], ['hundred', 100], ['middle', 25], ['null', null],
      ['undefined', undefined], ['text', '80'], ['nan', Number.NaN],
      ['positive infinity', Number.POSITIVE_INFINITY], ['negative infinity', Number.NEGATIVE_INFINITY],
      ['below zero', -0.01], ['above hundred', 100.01],
    ]
    const items = cases.map(([name, value], index) => ({
      ...monitor(index + 1, name, 0),
      availability_7d: value as number,
    }))
    const wrapper = mountGrid(items)

    expect(cardIds(wrapper)).toEqual([2, 3, 1, 4, 5, 6, 7, 8, 9, 10, 11])
    expect(cardValues(wrapper)).toEqual(['100', '25', '0', 'empty', 'empty', 'empty', 'empty', 'empty', 'empty', 'empty', 'empty'])
    wrapper.unmount()
  })

  it.each(['15d', '30d'] as const)('normalizes invalid %s details to the existing empty value', (window) => {
    const invalidValues = [
      null,
      undefined,
      '70',
      Number.NaN,
      Number.POSITIVE_INFINITY,
      Number.NEGATIVE_INFINITY,
      -1,
      101,
    ]
    const items = [monitor(1, 'valid 100', 0), ...invalidValues.map((_, index) => monitor(index + 2, `invalid ${index}`, 50))]
    const cache = reactive<Record<number, UserMonitorDetail>>({
      [items[0].id]: detail(items[0].id, 100, 100),
      ...items.slice(1).reduce<Record<number, UserMonitorDetail>>((result, item, index) => {
        result[item.id] = window === '15d'
          ? detail(item.id, invalidValues[index], 60)
          : detail(item.id, 60, invalidValues[index])
        return result
      }, {}),
    })
    const wrapper = mountGrid(items, window, cache)

    expect(cardIds(wrapper)).toEqual(items.map(item => item.id))
    expect(cardValues(wrapper)).toEqual(['100', ...invalidValues.map(() => 'empty')])
    wrapper.unmount()
  })

  it('reacts to selected-field updates and replacement arrays without changing the source data', async () => {
    const a = monitor(1, 'A', 70)
    const b = monitor(2, 'B', 60)
    const source = reactive([a, b])
    const initialSourceOrder = [...source]
    const originalA = source[0]
    const cache = reactive<Record<number, UserMonitorDetail>>({
      [a.id]: detail(a.id, 80, 10),
      [b.id]: detail(b.id, 70, 90),
    })
    const wrapper = mountGrid(source, '7d', cache)

    expect(cardIds(wrapper)).toEqual([a.id, b.id])
    expect(cardValues(wrapper)).toEqual(['70', '60'])
    source[0].availability_7d = 50
    const sourceOrderAfterProducerUpdate = [...source]
    const sourceFieldsAfterProducerUpdate = source.map(item => JSON.stringify(item))
    expect(sourceOrderAfterProducerUpdate).toEqual(initialSourceOrder)
    await nextTick()
    expect(cardIds(wrapper)).toEqual([b.id, a.id])
    expect(cardValues(wrapper)).toEqual(['60', '50'])

    cache[a.id].models[0].availability_15d = 95
    await nextTick()
    expect(cardIds(wrapper)).toEqual([b.id, a.id])
    expect(cardValues(wrapper)).toEqual(['60', '50'])

    await wrapper.setProps({ window: '15d' })
    expect(cardIds(wrapper)).toEqual([a.id, b.id])
    expect(cardValues(wrapper)).toEqual(['95', '70'])
    cache[a.id].models[0].availability_30d = 100
    await nextTick()
    expect(cardIds(wrapper)).toEqual([a.id, b.id])
    expect(cardValues(wrapper)).toEqual(['95', '70'])
    cache[a.id].models[0].availability_15d = 20
    await nextTick()
    expect(cardIds(wrapper)).toEqual([b.id, a.id])
    expect(cardValues(wrapper)).toEqual(['70', '20'])

    await wrapper.setProps({ window: '30d' })
    expect(cardIds(wrapper)).toEqual([a.id, b.id])
    expect(cardValues(wrapper)).toEqual(['100', '90'])
    cache[a.id].models[0].availability_30d = 5
    await nextTick()
    expect(cardIds(wrapper)).toEqual([b.id, a.id])
    expect(cardValues(wrapper)).toEqual(['90', '5'])

    const replacement = [monitor(3, 'C', 95), monitor(4, 'D', 10)]
    await wrapper.setProps({ window: '7d', items: replacement })
    expect(cardIds(wrapper)).toEqual([3, 4])
    expect(cardValues(wrapper)).toEqual(['95', '10'])
    expect(source[0]).toBe(sourceOrderAfterProducerUpdate[0])
    expect(source[1]).toBe(sourceOrderAfterProducerUpdate[1])
    expect(source.map(item => JSON.stringify(item))).toEqual(sourceFieldsAfterProducerUpdate)
    expect(source[0]).toBe(originalA)
    wrapper.unmount()
  })

  it('keeps unresolved cards in input order, then progressively follows details as they arrive', async () => {
    const a = monitor(1, 'A', 90)
    const b = monitor(2, 'B', 80)
    const c = monitor(3, 'C', 70)
    const cache = reactive<Record<number, UserMonitorDetail>>({})
    const wrapper = mountGrid([a, b, c], '15d', cache)

    expect(cardIds(wrapper)).toEqual([a.id, b.id, c.id])
    expect(cardValues(wrapper)).toEqual(['empty', 'empty', 'empty'])
    cache[b.id] = detail(b.id, 60, 0)
    await nextTick()
    expect(cardIds(wrapper)).toEqual([b.id, a.id, c.id])
    expect(cardValues(wrapper)).toEqual(['60', 'empty', 'empty'])
    cache[c.id] = detail(c.id, 80, 0)
    await nextTick()
    expect(cardIds(wrapper)).toEqual([c.id, b.id, a.id])
    cache[a.id] = detail(a.id, 90, 0)
    await nextTick()
    expect(cardIds(wrapper)).toEqual([a.id, c.id, b.id])
    wrapper.unmount()
  })

  it('puts a known zero before unknown values and matches details by the primary model', () => {
    const zero = monitor(1, 'Zero', 99)
    const unknown = monitor(2, 'Unknown', 100)
    const mismatched = monitor(3, 'Primary model match', 50)
    mismatched.primary_model = 'primary'
    const cache = reactive<Record<number, UserMonitorDetail>>({
      [zero.id]: detail(zero.id, 0, 0),
      [unknown.id]: {
        ...detail(unknown.id, 100, 100),
        models: [{ ...detail(unknown.id, 100, 100).models[0], model: 'other' }],
      },
      [mismatched.id]: {
        ...detail(mismatched.id, 10, 10),
        models: [
          { ...detail(mismatched.id, 100, 100).models[0], model: 'decoy' },
          { ...detail(mismatched.id, 25, 25).models[0], model: 'primary' },
        ],
      },
    })
    const wrapper = mountGrid([zero, unknown, mismatched], '15d', cache)

    expect(cardIds(wrapper)).toEqual([mismatched.id, zero.id, unknown.id])
    expect(cardValues(wrapper)).toEqual(['25', '0', 'empty'])
    wrapper.unmount()
  })

  it('passes the original item through reordered cards and preserves empty/loading/refresh states', async () => {
    const lower = monitor(1, 'Lower', 10)
    const higher = monitor(2, 'Higher', 90)
    const source = reactive([lower, higher])
    const expectedLower = source[0]
    const sourceOrder = [...source]
    const sourceFieldsSnapshot = source.map(item => JSON.stringify(item))
    const wrapper = mountGrid(source)

    expect(cardIds(wrapper)).toEqual([higher.id, lower.id])
    const lowerCard = wrapper.findAllComponents(cardStub).find(card => card.props('item').id === lower.id)
    expect(lowerCard).toBeDefined()
    expect(lowerCard!.props('item')).toBe(expectedLower)
    expect(lowerCard!.props('item').availability_7d).toBe(10)
    await wrapper.get('[data-id="1"]').trigger('click')
    expect(wrapper.emitted('cardClick')?.[0]?.[0]).toBe(expectedLower)
    expect(source[0]).toBe(sourceOrder[0])
    expect(source[1]).toBe(sourceOrder[1])
    expect(source.map(item => JSON.stringify(item))).toEqual(sourceFieldsSnapshot)

    await wrapper.setProps({ loading: true })
    expect(cardIds(wrapper)).toEqual([higher.id, lower.id])
    wrapper.unmount()

    const empty = mountGrid([], '7d', {}, false)
    expect(empty.find('.empty-state').exists()).toBe(true)
    empty.unmount()

    const skeleton = mountGrid([], '7d', {}, true)
    expect(skeleton.findAll('.animate-pulse')).toHaveLength(6)
    expect(skeleton.find('.empty-state').exists()).toBe(false)
    skeleton.unmount()

    await flushPromises()
  })
})
