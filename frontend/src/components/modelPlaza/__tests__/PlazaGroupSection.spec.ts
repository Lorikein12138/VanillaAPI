import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import PlazaGroupSection from '../PlazaGroupSection.vue'
import PlazaModelPricingTable from '../PlazaModelPricingTable.vue'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ cachedPublicSettings: null })
}))

function ladderModel(tiers: number): PlazaModel {
  const intervals = Array.from({ length: tiers }, (_, i) => ({
    min_tokens: i * 272000,
    max_tokens: i === tiers - 1 ? null : (i + 1) * 272000,
    tier_label: '',
    input_price: 5e-6,
    output_price: 3e-5,
    cache_write_price: null,
    cache_read_price: null,
    per_request_price: null
  }))
  return {
    name: 'gpt-5.6-sol',
    platform: 'openai',
    pricing: {
      billing_mode: 'token',
      input_price: 5e-6,
      output_price: 3e-5,
      cache_write_price: null,
      cache_read_price: null,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    },
    official_pricing: {
      input_price: 5e-6,
      output_price: 3e-5,
      cache_write_price: null,
      cache_read_price: null,
      intervals
    }
  }
}

function group(overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
  return {
    id: 1,
    name: 'g',
    description: '',
    platform: 'openai',
    subscription_type: 'standard',
    rate_multiplier: 1,
    peak_rate_enabled: false,
    peak_start: '',
    peak_end: '',
    peak_rate_multiplier: 1,
    is_exclusive: false,
    image_rate_independent: false,
    image_rate_multiplier: 1,
    video_rate_independent: false,
    video_rate_multiplier: 1,
    long_context_pricing_enabled: true,
    models: [ladderModel(2)],
    ...overrides
  }
}

function mountSection(g: ModelPlazaGroup) {
  return mount(PlazaGroupSection, {
    props: { group: g },
    global: {
      stubs: {
        GroupBadge: true,
        Icon: true,
        PlazaModelPricingTable: true
      }
    }
  })
}

/** 与 mountSection 相同但保留真实价格表,用于校验分组响应渲染出的行、倍率与单位。 */
function mountRealSection(g: ModelPlazaGroup) {
  return mount(PlazaGroupSection, {
    props: { group: g },
    global: {
      stubs: {
        GroupBadge: true,
        Icon: true
      }
    }
  })
}

function tokenModel(name: string, platform: string): PlazaModel {
  return {
    name,
    platform,
    pricing: {
      billing_mode: 'token',
      input_price: 3e-6,
      output_price: 1.5e-5,
      cache_write_price: null,
      cache_read_price: null,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: []
    },
    official_pricing: {
      input_price: 3e-6,
      output_price: 1.5e-5,
      cache_write_price: null,
      cache_read_price: null
    }
  }
}

function rowNames(wrapper: ReturnType<typeof mountRealSection>): string[] {
  return wrapper.findAll('tbody tr').map(row => row.find('td').text())
}

/** 首行最后一列为倍率列。 */
function rateText(wrapper: ReturnType<typeof mountRealSection>): string {
  return wrapper.find('tbody tr').findAll('td').at(-1)!.text()
}

const NOTE = 'modelPlaza.detail.longContextDisabledNote'

describe('PlazaGroupSection 长上下文说明', () => {
  it('passes video-specific rates to the pricing table', () => {
    const wrapper = mountSection(group({ video_rate_independent: true, video_rate_multiplier: 0 }))
    expect(wrapper.findComponent(PlazaModelPricingTable).props()).toMatchObject({
      videoRateIndependent: true,
      videoRateMultiplier: 0
    })
    wrapper.unmount()
  })
  it('分组关闭阶梯且组内有官方阶梯模型时显示说明', () => {
    const wrapper = mountSection(group({ long_context_pricing_enabled: false }))
    expect(wrapper.text()).toContain(NOTE)
  })

  it('分组开启阶梯时不显示', () => {
    const wrapper = mountSection(group({ long_context_pricing_enabled: true }))
    expect(wrapper.text()).not.toContain(NOTE)
  })

  it('分组关闭但没有官方阶梯模型时不显示', () => {
    const wrapper = mountSection(
      group({ long_context_pricing_enabled: false, models: [ladderModel(1)] })
    )
    expect(wrapper.text()).not.toContain(NOTE)
  })

  it('旧后端缺少开关字段时不显示', () => {
    const g = group()
    delete (g as Partial<ModelPlazaGroup>).long_context_pricing_enabled
    const wrapper = mountSection(g)
    expect(wrapper.text()).not.toContain(NOTE)
  })
})

describe('PlazaGroupSection 高峰配置传递', () => {
  it('分组启用高峰时把窗口描述与倍率传给价格表', () => {
    const wrapper = mountSection(
      group({
        subscription_type: 'subscription',
        peak_rate_enabled: true,
        peak_start: '14:00',
        peak_end: '18:00',
        peak_rate_multiplier: 1.5
      })
    )
    const table = wrapper.findComponent(PlazaModelPricingTable)
    // appStore mock 无 server_utc_offset,窗口描述不带时区标注
    expect(table.props('peakWindow')).toBe('14:00-18:00 ×1.5')
    expect(table.props('peakRateMultiplier')).toBe(1.5)
  })

  it('分组未启用高峰时窗口描述为空串', () => {
    const wrapper = mountSection(group())
    expect(wrapper.findComponent(PlazaModelPricingTable).props('peakWindow')).toBe('')
  })

  it('把分组视频独立倍率配置原值传给价格表', () => {
    const wrapper = mountSection(
      group({ video_rate_independent: true, video_rate_multiplier: 0.25 })
    )
    const table = wrapper.findComponent(PlazaModelPricingTable)
    expect(table.props('videoRateIndependent')).toBe(true)
    expect(table.props('videoRateMultiplier')).toBe(0.25)
  })
})

// Max 组响应只含基础模型、AG 组响应只含 thinking 模型:两组各自渲染、互不串入,倍率与单位随本组。
describe('PlazaGroupSection Max 与 AG thinking 分组响应', () => {
  function maxGroup(overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
    return group({
      id: 1,
      name: 'Claude Max',
      platform: 'anthropic',
      rate_multiplier: 1,
      models: [tokenModel('claude-opus-4-6', 'anthropic')],
      ...overrides
    })
  }

  function agGroup(overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
    return group({
      id: 2,
      name: 'Antigravity',
      platform: 'antigravity',
      rate_multiplier: 0.8,
      models: [tokenModel('claude-opus-4-6-thinking', 'antigravity')],
      ...overrides
    })
  }

  it('Max 分组只渲染基础模型行,AG 分组渲染 thinking 行,互不混入', () => {
    expect(rowNames(mountRealSection(maxGroup()))).toEqual(['claude-opus-4-6'])
    expect(rowNames(mountRealSection(agGroup()))).toEqual(['claude-opus-4-6-thinking'])
  })

  it('各分组按自身组倍率折算价格与倍率列,行数不变', () => {
    const max = mountRealSection(maxGroup())
    const ag = mountRealSection(agGroup())
    expect(rateText(max)).toBe('1x')
    expect(max.text()).not.toContain('$2.40')
    expect(max.findAll('tbody tr')).toHaveLength(1)
    expect(rateText(ag)).toBe('0.8x')
    // 输入价 3e-6 × 0.8 → $2.40(官方价列不乘倍率,恒为 $3.00)
    expect(ag.text()).toContain('$2.40')
    expect(ag.findAll('tbody tr')).toHaveLength(1)
  })

  it('专属倍率只作用于它所在的分组:划线展示原倍率,另一组不受影响', () => {
    const max = mountRealSection(maxGroup({ user_rate_multiplier: 0.5 }))
    const ag = mountRealSection(agGroup())
    expect(max.find('tbody tr .line-through').text()).toBe('1x')
    expect(rateText(max)).toContain('0.5x')
    expect(max.text()).toContain('$1.50')
    expect(rateText(ag)).toBe('0.8x')
  })

  it('token 分组的 thinking 行单位保持每百万 token,不因名称变成按次', () => {
    const text = mountRealSection(agGroup()).text()
    expect(text).toContain('modelPlaza.table.unitPerMillion')
    expect(text).not.toContain('modelPlaza.table.perUnitRequest')
    expect(text).not.toContain('modelPlaza.table.perUnitSecond')
  })

  it('AG 分组内的按次 thinking 模型按次单位 × 本组倍率展示', () => {
    const perRequest: PlazaModel = {
      name: 'claude-opus-4-6-thinking',
      platform: 'antigravity',
      pricing: {
        billing_mode: 'per_request',
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: null,
        image_output_price: null,
        per_request_price: 0.04,
        intervals: []
      },
      official_pricing: null
    }
    const wrapper = mountRealSection(agGroup({ rate_multiplier: 0.5, models: [perRequest] }))
    // 0.04 × 0.5 = 0.02;倍率列随本组 0.5x
    expect(wrapper.text()).toContain('$0.02')
    expect(wrapper.text()).toContain('modelPlaza.table.perUnitRequest')
    expect(rateText(wrapper)).toBe('0.5x')
  })
})
