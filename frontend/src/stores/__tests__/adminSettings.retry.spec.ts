import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useAdminSettingsStore } from '../adminSettings'

const mocks = vi.hoisted(() => ({ getSettings: vi.fn(), getConfig: vi.fn() }))
vi.mock('@/api', () => ({ adminAPI: { settings: { getSettings: mocks.getSettings }, payment: { getConfig: mocks.getConfig } } }))
beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  setActivePinia(createPinia())
  mocks.getSettings.mockResolvedValue({ ops_monitoring_enabled: true, custom_menu_items: [{ id: 'custom', title: 'Custom' }] })
  mocks.getConfig.mockResolvedValue({ data: { enabled: true } })
  vi.spyOn(console, 'error').mockImplementation(() => {})
})
afterEach(() => { vi.restoreAllMocks(); localStorage.clear() })

describe('admin settings fetch retry', () => {
  it.each(['settings', 'payment'])('retries a failed initial %s request without forcing a refresh', async (source) => {
    const request = source === 'settings' ? mocks.getSettings : mocks.getConfig
    request.mockRejectedValueOnce(new Error('Temporarily unavailable'))
    const store = useAdminSettingsStore()
    await store.fetch()
    expect(store.loaded).toBe(false)
    expect(store.loading).toBe(false)
    await store.fetch()
    expect(mocks.getSettings).toHaveBeenCalledTimes(2)
    expect(mocks.getConfig).toHaveBeenCalledTimes(2)
    expect(store.loaded).toBe(true)
    expect(store.paymentEnabled).toBe(true)
    expect(store.customMenuItems).toEqual([{ id: 'custom', title: 'Custom' }])
  })

  it('keeps cached values visible when the initial request fails', async () => {
    localStorage.setItem('ops_monitoring_enabled_cached', 'false')
    localStorage.setItem('payment_enabled_cached', 'true')
    mocks.getSettings.mockRejectedValueOnce(new Error('Offline'))
    const store = useAdminSettingsStore()
    await store.fetch()
    expect(store.opsMonitoringEnabled).toBe(false)
    expect(store.paymentEnabled).toBe(true)
    expect(localStorage.getItem('payment_enabled_cached')).toBe('true')
  })

  it('still reuses successfully loaded settings', async () => {
    const store = useAdminSettingsStore()
    await store.fetch()
    await store.fetch()
    expect(mocks.getSettings).toHaveBeenCalledTimes(1)
    expect(mocks.getConfig).toHaveBeenCalledTimes(1)
  })

  it('confirms the adaptive switch independently when the payment request fails', async () => {
    mocks.getSettings.mockResolvedValueOnce({ gateway_adaptive_scheduling_enabled: true })
    mocks.getConfig.mockRejectedValueOnce(new Error('Payment unavailable'))
    const store = useAdminSettingsStore()
    expect(store.gatewayAdaptiveSchedulingEnabled).toBeNull()
    await store.fetch()
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(true)
    expect(store.loaded).toBe(false)
  })

  it('keeps a saved adaptive value when an earlier GET resolves later', async () => {
    let resolveSettings!: (value: { gateway_adaptive_scheduling_enabled: boolean }) => void
    mocks.getSettings.mockImplementationOnce(() => new Promise((resolve) => { resolveSettings = resolve }))
    const store = useAdminSettingsStore()
    const request = store.fetch()
    const duplicate = store.fetch(true)
    store.setGatewayAdaptiveSchedulingEnabled(true)
    resolveSettings({ gateway_adaptive_scheduling_enabled: false })
    await Promise.all([request, duplicate])
    expect(mocks.getSettings).toHaveBeenCalledTimes(1)
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(true)
    store.setGatewayAdaptiveSchedulingEnabled(false)
    expect(store.gatewayAdaptiveSchedulingEnabled).toBe(false)
  })
})
