import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'

import AccountsView from '../AccountsView.vue'

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  platformOptions,
  showError,
  showWarning
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  platformOptions: [] as Array<{ value: string; label: string }>,
  showError: vi.fn(),
  showWarning: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      getById: vi.fn(),
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
      refreshCredentials: vi.fn(),
      duplicate: vi.fn(),
      createSparkShadow: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showWarning, showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('@/constants/platforms', () => ({ CONCRETE_PLATFORM_OPTIONS: platformOptions }))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: `
    <div>
      <div v-for="row in data" :key="row.id" data-test="account-row">
        <slot name="cell-select" :row="row" />
      </div>
    </div>
  `
})

const AccountTableFiltersStub = defineComponent({
  emits: ['update:filters', 'change'],
  setup(_, { emit }) {
    return () => h('button', {
      'data-test': 'apply-filters',
      onClick: () => {
        emit('update:filters', { platform: 'custom-provider', status: 'error', search: 'needle', group: '13' })
        emit('change')
      }
    }, 'apply')
  }
})

const AccountBulkActionsBarStub = defineComponent({
  props: { selectedIds: { type: Array, default: () => [] } },
  template: '<div data-test="selection-count">{{ selectedIds.length }}</div>'
})

function mountView() {
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: AccountTableFiltersStub,
        AccountBulkActionsBar: AccountBulkActionsBarStub,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        AccountPriorityCell: true,
        HelpTooltip: true,
        Icon: true,
        TotpStepUpDialog: true
      }
    }
  })
}

const accountRow = {
  id: 42,
  name: 'filter test account',
  platform: 'custom-provider',
  type: 'apikey',
  status: 'active',
  schedulable: true,
  concurrency: 1,
  priority: 1,
  group_ids: [13],
  extra: {},
  credentials: {}
}

describe('admin AccountsView filter persistence', () => {
  beforeEach(() => {
    localStorage.clear()
    platformOptions.splice(0, platformOptions.length, { value: 'custom-provider', label: 'Custom Provider' })
    listAccounts.mockReset().mockResolvedValue({ items: [accountRow], total: 1, page: 1, page_size: 20, pages: 1 })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: null, data: null })
    getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
    getAllProxies.mockReset().mockResolvedValue([])
    getAllGroups.mockReset().mockResolvedValue([])
    showError.mockReset()
    showWarning.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
    localStorage.clear()
  })

  it('loads valid dynamic platform/status filters, saves changes, and keeps other filters and selection behavior', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'custom-provider', status: 'rate_limited' }))
    vi.useFakeTimers()
    const wrapper = mountView()
    await flushPromises()

    expect(listAccounts).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ platform: 'custom-provider', status: 'rate_limited', type: '', privacy_mode: '', group: '', search: '', lite: '1' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )

    const select = wrapper.get('[data-test="account-row"] input')
    await select.setValue(true)
    expect(wrapper.get('[data-test="selection-count"]').text()).toBe('1')

    await wrapper.get('[data-test="apply-filters"]').trigger('click')
    expect(localStorage.getItem('account-filter-values')).toBe(JSON.stringify({ platform: 'custom-provider', status: 'error' }))
    expect(wrapper.get('[data-test="selection-count"]').text()).toBe('0')

    await vi.advanceTimersByTimeAsync(350)
    await flushPromises()
    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      expect.objectContaining({ platform: 'custom-provider', status: 'error', search: 'needle', group: '13', type: '', privacy_mode: '', lite: '1' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    wrapper.unmount()
  })

  it.each([
    ['removed platform and legacy status', JSON.stringify({ platform: 'removed-provider', status: 'disabled' })],
    ['malformed JSON', '{"platform":']
  ])('falls back for %s', async (_name, value) => {
    localStorage.setItem('account-filter-values', value)
    const wrapper = mountView()
    await flushPromises()
    expect(listAccounts).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ platform: '', status: '' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    wrapper.unmount()
  })

  it('continues filtering when local storage read or write throws', async () => {
    const getItem = localStorage.getItem.bind(localStorage)
    vi.spyOn(localStorage, 'getItem').mockImplementation((key: string) => {
      if (key === 'account-filter-values') throw new Error('storage unavailable')
      return getItem(key)
    })
    const wrapper = mountView()
    await flushPromises()
    expect(listAccounts).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ platform: '', status: '' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )

    const setItem = localStorage.setItem.bind(localStorage)
    vi.spyOn(localStorage, 'setItem').mockImplementation((key: string, value: string) => {
      if (key === 'account-filter-values') throw new Error('storage unavailable')
      setItem(key, value)
    })
    vi.useFakeTimers()
    await wrapper.get('[data-test="apply-filters"]').trigger('click')
    await vi.advanceTimersByTimeAsync(350)
    await flushPromises()
    expect(listAccounts).toHaveBeenLastCalledWith(
      1,
      20,
      expect.objectContaining({ platform: 'custom-provider', status: 'error', search: 'needle', group: '13' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    wrapper.unmount()
  })
})
