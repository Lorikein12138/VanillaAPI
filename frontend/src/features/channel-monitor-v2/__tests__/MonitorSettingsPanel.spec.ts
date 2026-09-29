import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { MonitorConfig } from '@/api/channelMonitorV2'

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

const platformNames = [
  'anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi',
  'zhipu', 'deepseek', 'minimax', 'opencode_go', 'composite',
]

const baseConfig = (overrides: Partial<MonitorConfig> = {}): MonitorConfig => ({
  version: 1,
  enabled: true,
  refresh_interval_seconds: 300,
  platforms: platformNames.map((platform) => ({ platform, enabled: true, models: [] })),
  group_ids: [],
  group_scope_mode: 'all',
  synthetic_empty_buckets_enabled: false,
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

describe('MonitorSettingsPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    getConfig.mockResolvedValue(baseConfig())
    getGroups.mockResolvedValue(groups)
    updateConfig.mockImplementation(async (config: MonitorConfig) => ({
      ...JSON.parse(JSON.stringify(config)) as MonitorConfig,
      version: 2,
    }))
  })

  it('shows exactly the configured supported platforms without Kiro', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('Claude')
    expect(wrapper.text()).toContain('Kimi')
    expect(wrapper.text()).toContain('Zhipu GLM')
    expect(wrapper.text()).toContain('DeepSeek')
    expect(wrapper.text()).toContain('MiniMax')
    expect(wrapper.text()).toContain('OpenCode')
    expect(wrapper.text()).toContain('Composite')
    expect(wrapper.text()).not.toContain('Kiro')
    expect(wrapper.findAll('input[type="text"]')).toHaveLength(11)
    wrapper.unmount()
  })

  it('places the synthetic data toggle below aggregation frequency and persists it', async () => {
    const wrapper = mountPanel()
    await flushPromises()

    const settingsRows = wrapper.findAll('.card.divide-y > div')
    expect(settingsRows[1].text()).toContain('channelMonitorV2.settings.refreshTitle')
    expect(settingsRows[2].text()).toContain('channelMonitorV2.settings.syntheticEmptyBucketsTitle')
    const switches = wrapper.findAll('[role="switch"]')
    expect(switches[1].attributes('aria-checked')).toBe('false')
    await switches[1].trigger('click')
    await wrapper.get('button.btn-primary').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      synthetic_empty_buckets_enabled: true,
    }))
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
      synthetic_empty_buckets_enabled: undefined,
    }))
    const wrapper = mountPanel()
    await flushPromises()

    expect(wrapper.text()).toContain('channelMonitorV2.settings.groupsAll')
    expect(wrapper.text()).not.toContain('channelMonitorV2.settings.groupsScopeEmpty')
    expect(wrapper.findAll('[role="switch"]')[1].attributes('aria-checked')).toBe('false')
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
