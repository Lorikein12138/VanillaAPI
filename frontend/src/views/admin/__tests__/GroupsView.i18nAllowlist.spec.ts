import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import GroupI18nField from '@/components/admin/group/GroupI18nField.vue'
import type { AdminGroup } from '@/types'
import GroupsView from '@/views/admin/GroupsView.vue'

const {
  listGroups,
  duplicateGroup,
  createGroup,
  updateGroup,
  getModelAllowlistCandidates,
  getUsageSummary,
  getCapacitySummary,
  getLiveCapability,
  showSuccess,
  showError
} = vi.hoisted(() => ({
  listGroups: vi.fn(),
  duplicateGroup: vi.fn(),
  createGroup: vi.fn(),
  updateGroup: vi.fn(),
  getModelAllowlistCandidates: vi.fn(),
  getUsageSummary: vi.fn(),
  getCapacitySummary: vi.fn(),
  getLiveCapability: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

const authState = vi.hoisted(() => ({ isSimpleMode: false }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      duplicate: duplicateGroup,
      getModelAllowlistCandidates,
      getUsageSummary,
      getCapacitySummary,
      getLiveCapability,
      getAll: vi.fn(),
      create: createGroup,
      update: updateGroup,
      delete: vi.fn(),
      updateSortOrder: vi.fn()
    },
    accounts: {
      list: vi.fn(),
      getById: vi.fn()
    }
  }
}))

vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess, showError }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => authState }))
vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({ isCurrentStep: vi.fn(() => false), nextStep: vi.fn() })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const sourceGroup = {
  id: 42,
  name: '标准组',
  description: '原始说明',
  i18n: { en: { name: 'Standard', description: 'Pro pool' } },
  platform: 'openai',
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: 'active',
  subscription_type: 'standard',
  daily_limit_usd: null,
  weekly_limit_usd: null,
  monthly_limit_usd: null,
  allow_image_generation: false,
  allow_batch_image_generation: false,
  image_rate_independent: false,
  image_rate_multiplier: 1,
  batch_image_discount_multiplier: 0.5,
  batch_image_hold_multiplier: 0.6,
  image_price_1k: null,
  image_price_2k: null,
  image_price_4k: null,
  video_rate_independent: false,
  video_rate_multiplier: 1,
  video_price_480p: null,
  video_price_720p: null,
  video_price_1080p: null,
  web_search_price_per_call: null,
  peak_rate_enabled: false,
  peak_start: '',
  peak_end: '',
  peak_rate_multiplier: 1,
  claude_code_only: false,
  fallback_group_id: null,
  fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false,
  default_mapped_model: '',
  messages_dispatch_model_config: undefined,
  require_oauth_only: false,
  require_privacy_set: false,
  created_at: '2026-07-16T00:00:00Z',
  updated_at: '2026-07-16T00:00:00Z',
  model_routing: null,
  model_routing_enabled: false,
  mcp_xml_inject: true,
  supported_model_scopes: [],
  account_count: 1,
  active_account_count: 1,
  rate_limited_account_count: 0,
  model_allowlist: { enabled: true, models: [], source_account_count: 2 },
  model_pricing: [{
    platform: 'openai',
    models: ['gpt-5.5'],
    billing_mode: 'token',
    input_price: 3e-6,
    output_price: 15e-6,
    cache_write_price: null,
    cache_write_1h_price: null,
    cache_read_price: null,
    image_input_price: null,
    image_output_price: null,
    per_request_price: null,
    reasoning_effort_multipliers: null,
    intervals: [],
    time_pricing: null
  }],
  sort_order: 10
} as AdminGroup

const AppLayoutStub = defineComponent({ template: '<main><slot /></main>' })
const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>'
})
const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
    columns: { type: Array, default: () => [] },
    loading: { type: Boolean, default: false }
  },
  template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div></div>'
})
const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

function mountView() {
  return mount(GroupsView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        Pagination: true,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        PlatformIcon: true,
        Icon: true,
        GroupCapacityBadge: true,
        GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true,
        VueDraggable: true
      }
    }
  })
}

function page(items: AdminGroup[]) {
  return { items, total: items.length, page: 1, page_size: 20, pages: 1 }
}

async function enterEnglishTranslation(form: ReturnType<typeof mountView>, fieldIndex: number, value: string) {
  const formFields = form.get('#create-group-form, #edit-group-form')
  const fields = formFields.findAllComponents(GroupI18nField)
  const field = fields[fieldIndex]
  await field.get('[data-testid="group-i18n-menu"]').trigger('click')
  await field.get('button[data-locale="en"]').trigger('click')
  const control = field.find('input, textarea')
  await control.setValue(value)
}

describe('GroupsView group translations and automatic allowlists', () => {
  beforeEach(() => {
    authState.isSimpleMode = false
    localStorage.clear()
    for (const fn of [
      listGroups,
      duplicateGroup,
      createGroup,
      updateGroup,
      getModelAllowlistCandidates,
      getUsageSummary,
      getCapacitySummary,
      getLiveCapability,
      showSuccess,
      showError
    ]) fn.mockReset()

    listGroups.mockResolvedValue(page([sourceGroup]))
    getModelAllowlistCandidates.mockResolvedValue([])
    getUsageSummary.mockResolvedValue([])
    getCapacitySummary.mockResolvedValue([])
    getLiveCapability.mockResolvedValue({ supported: false })
    createGroup.mockResolvedValue(sourceGroup)
    updateGroup.mockResolvedValue(sourceGroup)
  })

  afterEach(() => vi.restoreAllMocks())

  it('submits translated create fields while preserving the automatic allowlist setting', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.get('[data-tour="groups-create-btn"]').trigger('click')
      await flushPromises()
      const form = wrapper.get('#create-group-form')
      const fields = form.findAllComponents(GroupI18nField)
      await fields[0].get('input').setValue('标准组')
      await fields[1].get('textarea').setValue('原始说明')
      await enterEnglishTranslation(wrapper, 0, 'Standard')
      await enterEnglishTranslation(wrapper, 1, 'Pro pool')

      const allowlistSection = wrapper.get('[data-testid="group-model-allowlist-manual"]').element.parentElement!
      allowlistSection.querySelector<HTMLButtonElement>('button[role="switch"]')!.click()
      await wrapper.vm.$nextTick()
      expect(wrapper.get('[data-testid="group-model-allowlist-auto"]').exists()).toBe(true)

      await form.trigger('submit')
      await flushPromises()

      expect(createGroup).toHaveBeenCalledWith(expect.objectContaining({
        name: '标准组',
        description: '原始说明',
        i18n: { en: { name: 'Standard', description: 'Pro pool' } },
        model_allowlist: { enabled: true, models: [] }
      }))
    } finally {
      wrapper.unmount()
    }
  })

  it('submits translated edits without overwriting automatic allowlists or model prices', async () => {
    const wrapper = mountView()
    try {
      await flushPromises()
      const editButton = wrapper.findAll('button').find(button => button.text().trim() === 'common.edit')
      expect(editButton).toBeTruthy()
      await editButton!.trigger('click')
      await flushPromises()
      const form = wrapper.get('#edit-group-form')
      const fields = form.findAllComponents(GroupI18nField)
      await fields[0].get('[data-testid="group-i18n-menu"]').trigger('click')
      await fields[0].get('button[data-locale="en"]').trigger('click')
      await fields[0].get('input').setValue('Standard v2')
      await fields[1].get('[data-testid="group-i18n-menu"]').trigger('click')
      await fields[1].get('button[data-locale="en"]').trigger('click')
      await fields[1].get('textarea').setValue('Pro pool v2')

      await form.trigger('submit')
      await flushPromises()

      expect(updateGroup).toHaveBeenCalledWith(42, expect.objectContaining({
        i18n: { en: { name: 'Standard v2', description: 'Pro pool v2' } },
        model_allowlist: { enabled: true, models: [] },
        model_pricing: [expect.objectContaining({
          platform: 'openai',
          models: ['gpt-5.5'],
          input_price: 3e-6,
          output_price: 15e-6
        })]
      }))
    } finally {
      wrapper.unmount()
    }
  })

  it('delegates copy translation rules to the duplicate endpoint and refreshes the copied record', async () => {
    const duplicate = {
      ...sourceGroup,
      id: 43,
      name: '标准组 (Copy)',
      i18n: { en: { description: 'Pro pool' } }
    } as AdminGroup
    listGroups.mockReset()
    listGroups.mockResolvedValueOnce(page([sourceGroup])).mockResolvedValueOnce(page([sourceGroup, duplicate]))
    duplicateGroup.mockResolvedValue(duplicate)

    const wrapper = mountView()
    try {
      await flushPromises()
      await wrapper.get('[data-testid="group-duplicate"]').trigger('click')
      await flushPromises()

      expect(duplicateGroup).toHaveBeenCalledWith(42)
      expect(updateGroup).not.toHaveBeenCalled()
      expect(listGroups).toHaveBeenCalledTimes(2)
      expect(duplicate.i18n).toEqual({ en: { description: 'Pro pool' } })
      expect(duplicate.model_pricing).toEqual(sourceGroup.model_pricing)
    } finally {
      wrapper.unmount()
    }
  })
})
