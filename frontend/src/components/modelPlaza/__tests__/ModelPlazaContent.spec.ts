import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { defineComponent, h, nextTick, type PropType } from 'vue'
import type { ModelPlazaGroup, ModelPlazaResponse, PlazaModel } from '@/api/modelPlaza'
import { modelPlazaAPI } from '@/api/modelPlaza'
import enMessages from '@/i18n/locales/en'
import zhMessages from '@/i18n/locales/zh'
import ModelPlazaContent from '../ModelPlazaContent.vue'

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isAuthenticated: false })
}))

const messages = { en: enMessages, zh: zhMessages }

const PlazaGroupSectionStub = defineComponent({
  name: 'PlazaGroupSection',
  props: {
    group: { type: Object as PropType<ModelPlazaGroup>, required: true }
  },
  setup(props) {
    return () => h('article', { 'data-group-id': String(props.group.id) }, [
      h('h2', props.group.name),
      h('output', `rate:${props.group.user_rate_multiplier ?? props.group.rate_multiplier}`),
      h('ul', props.group.models.map((model) => h('li', { key: model.name }, model.name)))
    ])
  }
})

function model(name: string, platform = 'openai'): PlazaModel {
  return { name, platform, pricing: null, official_pricing: null }
}

function plazaGroup(
  id: number,
  name: string,
  platform: string,
  rateMultiplier: number,
  modelNames: string[],
  userRateMultiplier?: number
): ModelPlazaGroup {
  return {
    id,
    name,
    description: '',
    platform,
    subscription_type: 'standard',
    rate_multiplier: rateMultiplier,
    ...(userRateMultiplier === undefined ? {} : { user_rate_multiplier: userRateMultiplier }),
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
    models: modelNames.map((name) => model(name, platform))
  }
}

function response(groups: ModelPlazaGroup[]): ModelPlazaResponse {
  return { description: '', groups }
}

function mountContent(
  data: ModelPlazaResponse,
  options: { embedded?: boolean; locale?: 'zh' | 'en' } = {}
) {
  const i18n = createI18n({
    legacy: false,
    locale: options.locale ?? 'en',
    fallbackLocale: 'en',
    messages
  })
  const wrapper = mount(ModelPlazaContent, {
    props: { response: data, loading: false, embedded: options.embedded ?? false },
    global: {
      plugins: [i18n],
      stubs: { Icon: true, PlazaGroupSection: PlazaGroupSectionStub }
    }
  })
  return { wrapper, i18n }
}

function filterRow(wrapper: VueWrapper, label: string): HTMLElement {
  const labelElement = Array.from(wrapper.element.querySelectorAll('span')).find(
    (element) => element.textContent?.trim() === label
  )
  if (!labelElement?.parentElement) throw new Error(`Filter row not found: ${label}`)
  return labelElement.parentElement
}

function buttonInRow(wrapper: VueWrapper, rowLabel: string, buttonLabel: string): HTMLButtonElement {
  const button = Array.from(filterRow(wrapper, rowLabel).querySelectorAll('button')).find(
    (element) => element.textContent?.trim() === buttonLabel
  )
  if (!button) throw new Error(`Button not found: ${rowLabel} / ${buttonLabel}`)
  return button
}

async function clickButton(wrapper: VueWrapper, rowLabel: string, buttonLabel: string): Promise<void> {
  buttonInRow(wrapper, rowLabel, buttonLabel).click()
  await nextTick()
}

function visibleGroupIds(wrapper: VueWrapper): string[] {
  return Array.from(wrapper.element.querySelectorAll('[data-group-id]')).map(
    (element) => element.getAttribute('data-group-id') ?? ''
  )
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('ModelPlazaContent filter integration', () => {
  it('uses effective rates, real FilterBar events, per-dimension resets, and stable result sorting', async () => {
    const getModelPlaza = vi.spyOn(modelPlazaAPI, 'getModelPlaza')
    const data = response([
      plazaGroup(10, 'Zulu', 'openai', 1, ['Alpha model', 'Needle Hero'], 0.5),
      plazaGroup(12, 'Alpha', 'deepseek', 0.5, ['Needle Alpha']),
      plazaGroup(13, 'Beta', 'kimi', 0.5, ['Beta model']),
      plazaGroup(14, 'Gamma', 'anthropic', 0.75, ['Gamma model'])
    ])
    const { wrapper } = mountContent(data)

    expect(visibleGroupIds(wrapper)).toEqual(['12', '13', '10', '14'])
    expect(wrapper.find('[data-group-id="10"] output').text()).toBe('rate:0.5')

    await clickButton(wrapper, 'Rate', '0.5x')
    expect(visibleGroupIds(wrapper)).toEqual(['12', '13', '10'])
    await clickButton(wrapper, 'Platform', 'OpenAI')
    expect(visibleGroupIds(wrapper)).toEqual(['10'])
    await clickButton(wrapper, 'Group', 'Zulu')
    expect(visibleGroupIds(wrapper)).toEqual(['10'])

    await clickButton(wrapper, 'Group', 'All')
    expect(visibleGroupIds(wrapper)).toEqual(['10'])
    expect(buttonInRow(wrapper, 'Platform', 'OpenAI').classList.contains('chip-tinted-active')).toBe(true)
    expect(buttonInRow(wrapper, 'Rate', '0.5x').classList.contains('bg-gray-950')).toBe(true)

    await clickButton(wrapper, 'Platform', 'All')
    expect(visibleGroupIds(wrapper)).toEqual(['12', '13', '10'])
    await clickButton(wrapper, 'Rate', 'All')
    expect(visibleGroupIds(wrapper)).toEqual(['12', '13', '10', '14'])

    await clickButton(wrapper, 'Platform', 'DeepSeek')
    expect(visibleGroupIds(wrapper)).toEqual(['12'])
    await clickButton(wrapper, 'Platform', 'All')
    await clickButton(wrapper, 'Platform', 'Kimi')
    expect(visibleGroupIds(wrapper)).toEqual(['13'])

    expect(getModelPlaza).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('searches models case-insensitively, shows no-results, and restores results when cleared', async () => {
    const getModelPlaza = vi.spyOn(modelPlazaAPI, 'getModelPlaza')
    const data = response([
      plazaGroup(1, 'OpenAI group', 'openai', 1, ['Alpha NEEDLE', 'Second model']),
      plazaGroup(2, 'Kimi group', 'kimi', 1, ['needle another'])
    ])
    const { wrapper } = mountContent(data, { locale: 'en' })

    expect(visibleGroupIds(wrapper)).toEqual(['2', '1'])
    await wrapper.find('input').setValue('NEEDLE')
    expect(visibleGroupIds(wrapper)).toEqual(['2', '1'])
    expect(wrapper.find('[data-group-id="1"] ul').text()).toBe('Alpha NEEDLE')
    expect(wrapper.find('[data-group-id="2"] ul').text()).toBe('needle another')

    await wrapper.find('input').setValue('nothing-matches')
    expect(visibleGroupIds(wrapper)).toEqual([])
    expect(wrapper.text()).toContain(enMessages.modelPlaza.noSearchResult)

    await wrapper.find('input').setValue('')
    expect(visibleGroupIds(wrapper)).toEqual(['2', '1'])
    expect(wrapper.find('[data-group-id="1"] ul').text()).toContain('Second model')
    expect(Array.from(wrapper.element.querySelectorAll('[role="group"]')).map((node) => node.getAttribute('aria-label'))).toEqual([
      'OpenAI', 'Chinese AI'
    ])
    expect(getModelPlaza).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it.each([false, true])('uses only the supplied visible groups when embedded=%s and response changes', async (embedded) => {
    const getModelPlaza = vi.spyOn(modelPlazaAPI, 'getModelPlaza')
    const firstResponse = response([
      plazaGroup(1, 'OpenAI visible', 'openai', 1, ['OpenAI model']),
      plazaGroup(2, 'Kimi visible', 'kimi', 0.5, ['Kimi model'])
    ])
    const restrictedResponse = response([
      plazaGroup(30, 'Gemini visible', 'gemini', 0.75, ['Gemini model'])
    ])
    const { wrapper } = mountContent(firstResponse, { embedded, locale: 'en' })

    expect(wrapper.find('h1').exists()).toBe(!embedded)
    expect(visibleGroupIds(wrapper)).toEqual(['2', '1'])
    expect(Array.from(wrapper.element.querySelectorAll('[role="group"]')).map((node) => node.getAttribute('aria-label'))).toEqual([
      'OpenAI', 'Chinese AI'
    ])

    await wrapper.setProps({ response: restrictedResponse })
    expect(visibleGroupIds(wrapper)).toEqual(['30'])
    expect(Array.from(wrapper.element.querySelectorAll('[role="group"]')).map((node) => node.getAttribute('aria-label'))).toEqual([
      'Other'
    ])
    expect(wrapper.text()).toContain('Gemini visible')
    expect(wrapper.text()).not.toContain('OpenAI visible')
    expect(wrapper.text()).not.toContain('Kimi visible')
    expect(getModelPlaza).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
