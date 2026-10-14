import { flushPromises, mount } from '@vue/test-utils'
import Select from '@/components/common/Select.vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { MonitorConfig } from '@/api/channelMonitorV2'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'
import {
  BUILTIN_PLATFORM_CATALOG,
  listPlatforms,
  resetPlatformCatalog,
  setPlatformCatalog,
} from '@/constants/platformCatalog'

const { getConfig, updateConfig, getGroups, showError, showSuccess } = vi.hoisted(() => ({
  getConfig: vi.fn(),
  updateConfig: vi.fn(),
  getGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api/channelMonitorV2', async (importOriginal) => ({
  ...await importOriginal<typeof import('@/api/channelMonitorV2')>(),
  getConfig,
  updateConfig,
}))
vi.mock('@/api/admin', () => ({
  adminAPI: { groups: { getAllIncludingInactive: getGroups } },
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    cachedPublicSettings: { channel_monitor_enabled: true },
    showError,
    showSuccess,
  }),
}))
vi.mock('@/utils/featureFlags', () => ({
  getChannelMonitorMode: () => 'v2',
  isChannelMonitorV2Mode: () => true,
}))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key, te: () => true }),
}))

import MonitorSettingsPanel from '../MonitorSettingsPanel.vue'

const platformNames = [...listPlatforms().map((platform) => platform.id), 'composite']

const baseConfig = (overrides: Partial<MonitorConfig> = {}): MonitorConfig => ({
  version: 1,
  enabled: true,
  refresh_interval_seconds: 300,
  platforms: platformNames.map((platform) => ({ platform, enabled: true, models: [] })),
  group_ids: [],
  group_scope_mode: 'all',
  health_thresholds: {
    minimum_sample: 50,
    warning_error_rate: 0.05,
    critical_error_rate: 0.2,
    target_ttft_ms: 3000,
    warning_ttft_ms: 3000,
    critical_ttft_ms: 10000,
    warning_cache_rate: 0,
    critical_cache_rate: 0,
    error_weight: 0.6,
    ttft_weight: 0.2,
    cache_weight: 0.2,
  },
  ignored_error_categories: [],
  ...overrides,
})

const groups = [
  { id: 1, name: 'Primary', platform: 'openai' },
  { id: 2, name: 'Secondary', platform: 'gemini' },
]

function mountPanel() {
  return mount(MonitorSettingsPanel)
}

function platformButton(wrapper: ReturnType<typeof mountPanel>, key: string) {
  return wrapper.findAll('button').find((item) => item.text() === `channelMonitorV2.settings.${key}`)!
}

async function addPlatform(wrapper: ReturnType<typeof mountPanel>, platform: string) {
  wrapper.findComponent(Select).vm.$emit('update:modelValue', platform)
  await flushPromises()
  await platformButton(wrapper, 'addPlatform').trigger('click')
}

describe('MonitorSettingsPanel', () => {
  afterEach(() => {
    resetPlatformCatalog()
  })

  beforeEach(() => {
    vi.clearAllMocks()
    getConfig.mockResolvedValue(baseConfig())
    getGroups.mockResolvedValue(groups)
    updateConfig.mockImplementation(async (config: MonitorConfig) => ({
      ...JSON.parse(JSON.stringify(config)) as MonitorConfig,
      version: 2,
    }))
  })

  it('shows all fourteen configured platforms using catalog labels without Kiro', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('Claude')
    expect(wrapper.text()).toContain('Kimi')
    expect(wrapper.text()).toContain('Zhipu GLM')
    expect(wrapper.text()).toContain('DeepSeek')
    expect(wrapper.text()).toContain('MiniMax')
    expect(wrapper.text()).toContain('OpenCode')
    expect(wrapper.text()).toContain('Composite')
    expect(wrapper.text()).toContain('TypeSafe / Jev')
    expect(wrapper.text()).toContain('Command Code')
    expect(wrapper.text()).toContain('Cline')
    expect(wrapper.text()).not.toContain('Kiro')
    expect(wrapper.findAll('input[type="text"]')).toHaveLength(14)
    wrapper.unmount()
  })

  it('preserves disabled new platform models and API order when saving', async () => {
    const platforms = baseConfig().platforms.slice().reverse().map((platform) => ({ ...platform }))
    const commandCode = platforms.find((platform) => platform.platform === 'command_code')!
    commandCode.enabled = false
    commandCode.models = ['command-custom-model']
    const cline = platforms.find((platform) => platform.platform === 'cline')!
    cline.enabled = false
    cline.models = ['cline-custom-model']
    getConfig.mockResolvedValue(baseConfig({ platforms }))

    const wrapper = mountPanel()
    await flushPromises()
    await wrapper.findAll('[role="switch"]')[0].trigger('click')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledTimes(1)
    expect(updateConfig.mock.calls[0][0].platforms).toEqual(platforms)
    expect(updateConfig.mock.calls[0][0].platforms.map((platform) => platform.platform)).toEqual(
      platforms.map((platform) => platform.platform),
    )
    wrapper.unmount()
  })

  it('uses the platform catalog display name for a newly registered platform', async () => {
    setPlatformCatalog({
      ...BUILTIN_PLATFORM_CATALOG,
      platforms: [
        ...BUILTIN_PLATFORM_CATALOG.platforms,
        { ...BUILTIN_PLATFORM_CATALOG.platforms[0], id: 'future_provider', display_name: 'Future Provider' },
      ],
    })
    getConfig.mockResolvedValue(baseConfig({
      platforms: [...baseConfig().platforms, { platform: 'future_provider', enabled: true, models: [] }],
    }))

    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('Future Provider')
    expect(wrapper.text()).not.toContain('future_provider')
    wrapper.unmount()
  })

  it('omits the retired toggle and drops archived response data from saves', async () => {
    getConfig.mockResolvedValue({ ...baseConfig(), synthetic_empty_buckets_enabled: true })
    const wrapper = mountPanel()
    await flushPromises()
    expect(wrapper.text()).not.toContain('channelMonitorV2.settings.syntheticEmptyBucketsTitle')
    const settingsRows = wrapper.findAll('.card.divide-y > div')
    expect(settingsRows).toHaveLength(2)
    expect(settingsRows[1].text()).toContain('channelMonitorV2.settings.refreshTitle')
    await wrapper.findAll('[role="switch"]')[0].trigger('click')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledTimes(1)
    expect(updateConfig.mock.calls[0][0]).not.toHaveProperty('synthetic_empty_buckets_enabled')
    expect(updateConfig.mock.calls[0][0].platforms).toEqual(baseConfig().platforms)
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('keeps selected-empty scope when the last selected group is unchecked', async () => {
    getConfig.mockResolvedValue(baseConfig({
      group_ids: [1],
      group_scope_mode: 'selected',
    }))
    getGroups.mockResolvedValue([groups[0]])
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.find('input[type="checkbox"]').trigger('change')
    expect(wrapper.text()).toContain('channelMonitorV2.settings.groupsScopeEmpty')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      group_scope_mode: 'selected',
      group_ids: [],
    }))
    wrapper.unmount()
  })

  it('reloads the saved scope after a 409 version conflict', async () => {
    getConfig.mockReset()
    getConfig
      .mockResolvedValueOnce(baseConfig({ group_ids: [1], group_scope_mode: 'selected' }))
      .mockResolvedValueOnce(baseConfig({ group_ids: [], group_scope_mode: 'selected' }))
    getGroups.mockResolvedValue([groups[0]])
    updateConfig.mockRejectedValueOnce({
      message: 'configuration changed',
      response: { status: 409, data: { detail: 'configuration changed' } },
    })
    const wrapper = mountPanel()
    await flushPromises()

    await wrapper.find('input[type="checkbox"]').trigger('change')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({ group_scope_mode: 'selected', group_ids: [] }))
    expect(getConfig).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('channelMonitorV2.settings.groupsScopeEmpty')
    expect(showError).toHaveBeenCalled()
    wrapper.unmount()
  })

  it('interprets a legacy empty group list as all groups', async () => {
    getConfig.mockResolvedValue(baseConfig({
      group_ids: [],
      group_scope_mode: undefined,
    }))
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('channelMonitorV2.settings.groupsAll')
    expect(wrapper.text()).not.toContain('channelMonitorV2.settings.groupsScopeEmpty')
    expect(wrapper.findAll('.card.divide-y > div')).toHaveLength(2)
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('adds a missing provider and saves its models without changing existing settings', async () => {
    const original = baseConfig({
      platforms: [
        { platform: 'openai', enabled: false, models: ['gpt-4'] },
        { platform: 'custom-provider', enabled: true, models: ['custom-model'] },
      ],
    })
    getConfig.mockResolvedValue(original)
    const wrapper = mountPanel()
    await flushPromises()

    expect(platformButton(wrapper, 'save').attributes('disabled')).toBeDefined()
    expect(wrapper.findComponent(Select).props('options')).not.toContainEqual(
      expect.objectContaining({ value: 'openai' }),
    )
    await wrapper.findComponent(Select).find('button').trigger('click')
    const option = Array.from(document.querySelectorAll<HTMLElement>('[role="option"]'))
      .find((item) => item.textContent?.trim() === 'DeepSeek')!
    option.click()
    await flushPromises()
    await platformButton(wrapper, 'addPlatform').trigger('click')

    expect(platformButton(wrapper, 'addPlatform').attributes('disabled')).toBeDefined()
    expect(wrapper.findComponent(Select).props('options')).not.toContainEqual(
      expect.objectContaining({ value: 'deepseek' }),
    )
    await wrapper.findAll('input[type="text"]').at(-1)!.setValue(' deepseek-chat, deepseek-reasoner, deepseek-chat ')
    await platformButton(wrapper, 'save').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith({
      ...original,
      platforms: [...original.platforms, {
        platform: 'deepseek', enabled: true, models: ['deepseek-chat', 'deepseek-reasoner'],
      }],
    })
    expect(platformButton(wrapper, 'save').attributes('disabled')).toBeDefined()
    expect(original.platforms).toHaveLength(2)

    const persisted = await updateConfig.mock.results[0].value
    wrapper.unmount()
    getConfig.mockResolvedValue(persisted)
    const reloaded = mountPanel()
    await flushPromises()
    expect((reloaded.findAll('input[type="text"]').at(-1)!.element as HTMLInputElement).value)
      .toBe('deepseek-chat, deepseek-reasoner')
    expect(reloaded.findComponent(Select).props('options')).not.toContainEqual(
      expect.objectContaining({ value: 'deepseek' }),
    )
    reloaded.unmount()
  })

  it('can start with no configured platforms and rejects duplicate additions', async () => {
    getConfig.mockResolvedValue(baseConfig({ platforms: [] }))
    const wrapper = mountPanel()
    await flushPromises()
    expect(platformButton(wrapper, 'addPlatform').attributes('disabled')).toBeDefined()

    await addPlatform(wrapper, 'kimi')
    await addPlatform(wrapper, 'kimi')
    await platformButton(wrapper, 'save').trigger('click')
    await flushPromises()

    expect(updateConfig.mock.calls[0][0].platforms).toEqual([
      { platform: 'kimi', enabled: true, models: [] },
    ])
    wrapper.unmount()
  })

  it('restores the server config and available options when saving fails', async () => {
    getConfig.mockResolvedValue(baseConfig({
      platforms: [
        { platform: 'openai', enabled: false, models: ['gpt-4'] },
        { platform: 'custom-provider', enabled: true, models: ['custom-model'] },
      ],
    }))
    const wrapper = mountPanel()
    await flushPromises()
    updateConfig.mockRejectedValueOnce(new Error('save failed'))

    await addPlatform(wrapper, 'minimax')
    await platformButton(wrapper, 'save').trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledTimes(1)
    expect(wrapper.findComponent(Select).props('options')).toContainEqual(
      expect.objectContaining({ value: 'minimax' }),
    )
    expect(wrapper.findAll('input[type="text"]')).toHaveLength(2)
    expect(platformButton(wrapper, 'save').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('hides the add control after every supported platform is configured', async () => {
    getConfig.mockResolvedValue(baseConfig({
      platforms: CONCRETE_PLATFORM_OPTIONS.map(({ value }) => ({ platform: value, enabled: true, models: [] })),
    }))
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.findComponent(Select).exists()).toBe(false)
    expect(platformButton(wrapper, 'addPlatform')).toBeUndefined()
    wrapper.unmount()
  })
})
