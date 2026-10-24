import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { defineComponent, nextTick } from 'vue'
import enMessages from '@/i18n/locales/en'
import zhMessages from '@/i18n/locales/zh'
import { platformDisplayName } from '@/constants/platformCatalog'
import PlazaFilterBar from '../PlazaFilterBar.vue'

type Locale = 'zh' | 'en'
type PlazaGroupOption = { id: number; name: string; platform: string; rate: number }

const messages = { en: enMessages, zh: zhMessages }

const filterLabels: Record<Locale, { platform: string; group: string; rate: string; model: string; all: string }> = {
  zh: { platform: '平台', group: '分组', rate: '倍率', model: '模型', all: '全部' },
  en: { platform: 'Platform', group: 'Group', rate: 'Rate', model: 'Model', all: 'All' }
}

const categoryLabels: Record<Locale, Record<string, string>> = {
  zh: { anthropic: 'Anthropic', openai: 'OpenAI', domestic: '国产模型', other: '其他' },
  en: { anthropic: 'Anthropic', openai: 'OpenAI', domestic: 'Chinese AI', other: 'Other' }
}

const platformNames = [
  ['anthropic', 'Anthropic'],
  ['deepseek', 'DeepSeek'],
  ['openai', 'OpenAI'],
  ['minimax', 'MiniMax'],
  ['gemini', 'Gemini'],
  ['grok', 'Grok'],
  ['kimi', 'Kimi'],
  ['zhipu', 'Zhipu']
] as const

function createMessagesI18n(locale: Locale) {
  return createI18n({
    legacy: false,
    locale,
    fallbackLocale: 'en',
    messages
  })
}

function group(id: number, name: string, platform: string, rate = 1): PlazaGroupOption {
  return { id, name, platform, rate }
}

function mountFilterBar(
  overrides: Partial<{
    platforms: string[]
    groups: PlazaGroupOption[]
    rates: number[]
    platform: string
    groupId: number | 'all'
    rate: number | 'all'
    search: string
  }> = {},
  locale: Locale = 'zh'
) {
  const i18n = createMessagesI18n(locale)
  const wrapper = mount(PlazaFilterBar, {
    props: {
      platforms: ['anthropic', 'openai'],
      groups: [group(1, 'Claude', 'anthropic'), group(2, 'GPT', 'openai')],
      rates: [1],
      platform: 'all',
      groupId: 'all',
      rate: 'all',
      search: '',
      ...overrides
    },
    global: {
      plugins: [i18n],
      stubs: {
        Icon: true,
        PlatformIcon: defineComponent({
          props: { platform: { type: String, default: '' }, size: { type: String, default: '' } },
          template: '<i data-test="platform-icon" :data-platform="platform" :data-size="size"></i>'
        })
      }
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

function groupNames(wrapper: VueWrapper, categoryLabel: string): string[] {
  const category = Array.from(wrapper.element.querySelectorAll('[role="group"]')).find(
    (element) => element.getAttribute('aria-label') === categoryLabel
  )
  return category
    ? Array.from(category.querySelectorAll('button')).map((button) => button.textContent?.trim() ?? '')
    : []
}

function mountedCategories(wrapper: VueWrapper): string[] {
  return Array.from(wrapper.element.querySelectorAll('[role="group"]')).map(
    (element) => element.getAttribute('aria-label') ?? ''
  )
}

function productionGroups(): PlazaGroupOption[] {
  return [
    group(1, 'Claude Kiro', 'anthropic'),
    group(2, 'Claude Antigravity', 'anthropic'),
    group(3, 'Claude Max Sale', 'anthropic'),
    group(4, 'Claude Max', 'anthropic'),
    group(5, 'Claude Max Ent', 'anthropic'),
    group(6, 'ChatGPT Sale', 'openai'),
    group(7, 'ChatGPT Plus Sale', 'openai'),
    group(8, 'ChatGPT Plus', 'openai'),
    group(9, 'ChatGPT Pro Sale', 'openai'),
    group(10, 'ChatGPT Pro', 'openai'),
    group(11, 'ChatGPT Key', 'openai'),
    group(12, 'ChatGPT Image', 'openai'),
    group(13, 'DeepSeek', 'deepseek'),
    group(14, 'GLM', 'zhipu'),
    group(15, 'Kimi', 'kimi'),
    group(16, 'MiniMax', 'minimax'),
    group(17, 'Gemini', 'gemini'),
    group(18, 'Grok', 'grok')
  ]
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('PlazaFilterBar platform labels', () => {
  it.each(['zh', 'en'] as const)('shows the eight exact brand names in %s', (locale) => {
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const ids = platformNames.map(([id]) => id)
    const { wrapper } = mountFilterBar({
      platforms: ids,
      groups: [group(1, 'DeepSeek group', 'deepseek')]
    }, locale)
    const row = filterRow(wrapper, filterLabels[locale].platform)
    const buttons = Array.from(row.querySelectorAll('button'))

    expect(buttons.map((button) => button.textContent?.trim())).toEqual([
      filterLabels[locale].all,
      ...platformNames.map(([, label]) => label)
    ])
    expect(Array.from(row.querySelectorAll('[data-test="platform-icon"]')).map((icon) => icon.getAttribute('data-platform'))).toEqual(ids)
    expect(warning.mock.calls.flat().join(' ')).not.toMatch(/Not found|missing.*translation/i)

    buttons[2].click()
    expect(wrapper.emitted('update:platform')).toEqual([['deepseek']])
    wrapper.unmount()
  })

  it('reacts to a runtime locale switch and keeps the shared catalog unchanged', async () => {
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const { wrapper, i18n } = mountFilterBar({
      platforms: ['openai', 'zhipu', 'antigravity', 'future_vendor'],
      groups: [group(1, 'OpenAI group', 'openai')]
    })

    expect(mountedCategories(wrapper)).toEqual(['OpenAI'])
    const row = filterRow(wrapper, '平台')
    expect(Array.from(row.querySelectorAll('button')).map((button) => button.textContent?.trim())).toEqual([
      '全部', 'OpenAI', 'Zhipu', 'Antigravity', 'future_vendor'
    ])
    expect(platformDisplayName('zhipu')).toBe('Zhipu GLM')

    i18n.global.locale.value = 'en'
    await nextTick()

    expect(mountedCategories(wrapper)).toEqual(['OpenAI'])
    expect(filterRow(wrapper, 'Group')).toBeTruthy()
    expect(wrapper.find('input').attributes('placeholder')).toBe('Search models')
    expect(warning.mock.calls.flat().join(' ')).not.toMatch(/Not found|missing.*translation/i)
    wrapper.unmount()
  })

  it('uses directory names for other registered platforms and preserves unknown IDs in events', () => {
    const { wrapper } = mountFilterBar({
      platforms: ['antigravity', 'future_vendor'],
      groups: [group(1, 'Future vendor group', 'future_vendor')]
    }, 'en')
    const row = filterRow(wrapper, 'Platform')

    expect(Array.from(row.querySelectorAll('button')).map((button) => button.textContent?.trim())).toEqual([
      'All', 'Antigravity', 'future_vendor'
    ])
    buttonInRow(wrapper, 'Platform', 'future_vendor').click()
    expect(wrapper.emitted('update:platform')).toEqual([['future_vendor']])
    wrapper.unmount()
  })

  it('treats prototype-key platform IDs as unknown: raw labels, no warnings, each group once under Other', () => {
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const ids = ['constructor', 'toString', '__proto__', 'hasOwnProperty']
    const { wrapper } = mountFilterBar({
      platforms: ids,
      groups: ids.map((id, index) => group(index + 1, `${id} group`, id))
    }, 'en')

    expect(Array.from(filterRow(wrapper, 'Platform').querySelectorAll('button')).map((button) => button.textContent?.trim())).toEqual([
      'All', ...ids
    ])
    expect(mountedCategories(wrapper)).toEqual(['Other'])
    expect(groupNames(wrapper, 'Other')).toEqual(ids.map((id) => `${id} group`))
    expect(warning.mock.calls.flat().join(' ')).not.toMatch(/Not found|missing.*translation/i)
    wrapper.unmount()
  })
})

describe('PlazaFilterBar group categories', () => {
  it.each(['zh', 'en'] as const)('renders all 18 configured groups in four named categories in %s', (locale) => {
    const { wrapper } = mountFilterBar({ groups: productionGroups() }, locale)
    const labels = categoryLabels[locale]

    expect(mountedCategories(wrapper)).toEqual([
      labels.anthropic, labels.openai, labels.domestic, labels.other
    ])
    expect(groupNames(wrapper, labels.anthropic)).toEqual([
      'Claude Kiro', 'Claude Antigravity', 'Claude Max Sale', 'Claude Max', 'Claude Max Ent'
    ])
    expect(groupNames(wrapper, labels.openai)).toEqual([
      'ChatGPT Sale', 'ChatGPT Plus Sale', 'ChatGPT Plus', 'ChatGPT Pro Sale',
      'ChatGPT Pro', 'ChatGPT Key', 'ChatGPT Image'
    ])
    expect(groupNames(wrapper, labels.domestic)).toEqual(['DeepSeek', 'GLM', 'Kimi', 'MiniMax'])
    expect(groupNames(wrapper, labels.other)).toEqual(['Gemini', 'Grok'])

    const groupsInDom = Array.from(wrapper.element.querySelectorAll('[role="group"] button'))
    expect(groupsInDom).toHaveLength(18)
    expect(groupsInDom.map((button) => button.textContent?.trim())).toHaveLength(new Set(groupsInDom.map((button) => button.textContent?.trim())).size)
    const groupRow = filterRow(wrapper, filterLabels[locale].group)
    const allGroupsButton = groupRow.querySelector('button')!
    const firstCategory = groupRow.querySelector('[role="group"]')!
    expect(allGroupsButton.textContent?.trim()).toBe(filterLabels[locale].all)
    expect(allGroupsButton.compareDocumentPosition(firstCategory) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(groupRow.querySelectorAll('[role="group"] button')[0]?.classList.contains('chip-tinted')).toBe(true)
    wrapper.unmount()
  })

  it('classifies arbitrary names, renamed groups, duplicate names, and platform counterexamples from platform only', async () => {
    const groups = [
      group(20, 'Claude Antigravity', 'anthropic'),
      group(21, 'Claude Antigravity', 'antigravity'),
      group(22, 'ChatGPT', 'kimi'),
      group(23, 'Arbitrary OpenAI', 'openai'),
      group(24, 'Arbitrary one', 'deepseek'),
      group(25, 'Arbitrary two', 'zhipu'),
      group(26, 'Arbitrary three', 'minimax'),
      group(27, 'Arbitrary four', 'gemini'),
      group(28, 'Arbitrary five', 'grok'),
      group(29, 'Composite model collection', 'composite'),
      group(30, 'Unknown configured platform', 'future_vendor')
    ]
    const original = structuredClone(groups)
    const { wrapper } = mountFilterBar({ groups }, 'en')

    expect(groupNames(wrapper, 'Anthropic')).toEqual(['Claude Antigravity'])
    expect(groupNames(wrapper, 'OpenAI')).toEqual(['Arbitrary OpenAI'])
    expect(groupNames(wrapper, 'Chinese AI')).toEqual(['ChatGPT', 'Arbitrary one', 'Arbitrary two', 'Arbitrary three'])
    expect(groupNames(wrapper, 'Other')).toEqual([
      'Claude Antigravity', 'Arbitrary four', 'Arbitrary five', 'Composite model collection', 'Unknown configured platform'
    ])
    expect(groups).toEqual(original)
    expect(wrapper.findAll('[role="group"] button')).toHaveLength(groups.length)

    const anthroButton = Array.from(wrapper.element.querySelectorAll('[role="group"][aria-label="Anthropic"] button'))[0] as HTMLButtonElement
    anthroButton.click()
    expect(wrapper.emitted('update:groupId')).toEqual([[20]])

    await wrapper.setProps({ groups: groups.map((item) => item.id === 22 ? { ...item, name: 'Renamed without provider hint' } : item) })
    expect(groupNames(wrapper, 'Chinese AI')).toEqual([
      'Renamed without provider hint', 'Arbitrary one', 'Arbitrary two', 'Arbitrary three'
    ])
    wrapper.unmount()
  })

  it('adds and removes nonempty categories as props change, without empty placeholders', async () => {
    const { wrapper } = mountFilterBar({ groups: [group(1, 'OpenAI first', 'openai')] }, 'zh')
    expect(mountedCategories(wrapper)).toEqual(['OpenAI'])

    await wrapper.setProps({ groups: [group(1, 'OpenAI first', 'openai'), group(2, 'First domestic', 'deepseek')] })
    expect(mountedCategories(wrapper)).toEqual(['OpenAI', '国产模型'])
    expect(groupNames(wrapper, '国产模型')).toEqual(['First domestic'])

    await wrapper.setProps({ groups: [group(1, 'OpenAI first', 'openai')] })
    expect(mountedCategories(wrapper)).toEqual(['OpenAI'])

    await wrapper.setProps({ groups: [] })
    expect(mountedCategories(wrapper)).toEqual([])
    expect(buttonInRow(wrapper, '分组', '全部').disabled).toBe(false)
    wrapper.unmount()
  })

  it('shows only the present category and keeps every filtered group visible when disabled', () => {
    const { wrapper } = mountFilterBar({
      groups: [group(1, 'DeepSeek', 'deepseek'), group(2, 'Kimi', 'kimi')],
      platform: 'openai',
      rate: 2
    }, 'zh')

    expect(mountedCategories(wrapper)).toEqual(['国产模型'])
    const domestic = wrapper.element.querySelector('[role="group"][aria-label="国产模型"]')!
    const buttons = Array.from(domestic.querySelectorAll('button')) as HTMLButtonElement[]
    expect(buttons.map((button) => button.disabled)).toEqual([true, true])
    expect(buttonInRow(wrapper, '分组', '全部').disabled).toBe(false)
    wrapper.unmount()
  })
})

describe('PlazaFilterBar existing filter contracts', () => {
  it('preserves cross-dimension availability, active styles, disabled behavior, and raw IDs', () => {
    const { wrapper } = mountFilterBar({
      platforms: ['deepseek', 'kimi', 'openai'],
      groups: [
        group(2, 'DeepSeek group', 'deepseek', 0.5),
        group(3, 'Kimi group', 'kimi', 0.5),
        group(4, 'OpenAI group', 'openai', 1)
      ],
      rates: [0.5, 1],
      platform: 'deepseek',
      groupId: 2,
      rate: 0.5
    }, 'en')

    const platformDeepSeek = buttonInRow(wrapper, 'Platform', 'DeepSeek')
    const platformKimi = buttonInRow(wrapper, 'Platform', 'Kimi')
    const platformOpenAI = buttonInRow(wrapper, 'Platform', 'OpenAI')
    expect(platformDeepSeek.classList.contains('chip-tinted-active')).toBe(true)
    expect(platformKimi.disabled).toBe(true)
    expect(platformOpenAI.disabled).toBe(true)

    const groupDeepSeek = buttonInRow(wrapper, 'Group', 'DeepSeek group')
    const groupKimi = buttonInRow(wrapper, 'Group', 'Kimi group')
    const groupOpenAI = buttonInRow(wrapper, 'Group', 'OpenAI group')
    expect(groupDeepSeek.classList.contains('chip-tinted-active')).toBe(true)
    expect(groupDeepSeek.style.getPropertyValue('--chip-accent')).not.toBe('')
    expect(groupKimi.disabled).toBe(true)
    expect(groupOpenAI.disabled).toBe(true)

    expect(buttonInRow(wrapper, 'Rate', '0.5x').classList.contains('bg-gray-950')).toBe(true)
    expect(buttonInRow(wrapper, 'Rate', '1x').disabled).toBe(true)

    platformKimi.click()
    groupKimi.click()
    buttonInRow(wrapper, 'Rate', '1x').click()
    expect(wrapper.emitted()).toEqual({})
    platformDeepSeek.click()
    expect(wrapper.emitted('update:platform')).toEqual([['deepseek']])
    wrapper.unmount()
  })

  it('keeps each All action enabled and scoped to its own update event', () => {
    const { wrapper } = mountFilterBar({
      platforms: ['openai'],
      groups: [group(1, 'OpenAI group', 'openai', 0.5)],
      rates: [0.5],
      platform: 'openai',
      groupId: 1,
      rate: 0.5
    }, 'en')

    for (const label of ['Platform', 'Group', 'Rate']) {
      expect(buttonInRow(wrapper, label, 'All').disabled).toBe(false)
    }
    buttonInRow(wrapper, 'Platform', 'All').click()
    buttonInRow(wrapper, 'Group', 'All').click()
    buttonInRow(wrapper, 'Rate', 'All').click()

    expect(wrapper.emitted('update:platform')).toEqual([['all']])
    expect(wrapper.emitted('update:groupId')).toEqual([['all']])
    expect(wrapper.emitted('update:rate')).toEqual([['all']])
    expect(wrapper.emitted('update:search')).toBeUndefined()
    wrapper.unmount()
  })

  it('preserves input and clear-search events', async () => {
    const { wrapper } = mountFilterBar({ search: 'initial' }, 'en')
    const input = wrapper.find('input')

    await input.setValue('Claude Opus')
    expect(wrapper.emitted('update:search')).toEqual([['Claude Opus']])
    const clearButton = Array.from(input.element.parentElement!.querySelectorAll('button'))[0]
    clearButton.click()
    await nextTick()
    expect(wrapper.emitted('update:search')).toEqual([['Claude Opus'], ['']])
    wrapper.unmount()
  })
})
