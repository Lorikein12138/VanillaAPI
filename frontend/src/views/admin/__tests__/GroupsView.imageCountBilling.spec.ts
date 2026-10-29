import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AdminGroup } from '@/types'
import GroupsView from '@/views/admin/GroupsView.vue'

const { listGroups, createGroup, updateGroup, duplicateGroup, getModelAllowlistCandidates, getUsageSummary, getCapacitySummary, getLiveCapability } = vi.hoisted(() => ({
  listGroups: vi.fn(), createGroup: vi.fn(), updateGroup: vi.fn(), getModelAllowlistCandidates: vi.fn(),
  duplicateGroup: vi.fn(),
  getUsageSummary: vi.fn(), getCapacitySummary: vi.fn(), getLiveCapability: vi.fn(),
}))
const authState = vi.hoisted(() => ({ isSimpleMode: false }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: {
      list: listGroups, create: createGroup, update: updateGroup,
      getModelAllowlistCandidates, getUsageSummary, getCapacitySummary, getLiveCapability,
      getAll: vi.fn(), delete: vi.fn(), updateSortOrder: vi.fn(), duplicate: duplicateGroup,
    },
    accounts: { list: vi.fn(), getById: vi.fn() },
  },
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => authState }))
vi.mock('@/stores/onboarding', () => ({ useOnboardingStore: () => ({ isCurrentStep: vi.fn(() => false), nextStep: vi.fn() }) }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const group: AdminGroup = {
  id: 42, name: 'Image group', description: null, platform: 'openai', rate_multiplier: 1, rpm_limit: 0,
  is_exclusive: false, status: 'active', subscription_type: 'standard', daily_limit_usd: null,
  weekly_limit_usd: null, monthly_limit_usd: null, allow_image_generation: true,
  allow_batch_image_generation: false, image_count_billing: false, image_rate_independent: false,
  image_rate_multiplier: 1, batch_image_discount_multiplier: 0.5, batch_image_hold_multiplier: 0.6,
  image_price_1k: null, image_price_2k: null, image_price_4k: null, video_rate_independent: false,
  video_rate_multiplier: 1, video_price_480p: null, video_price_720p: null, video_price_1080p: null,
  web_search_price_per_call: null, peak_rate_enabled: false, peak_start: '', peak_end: '', peak_rate_multiplier: 1,
  claude_code_only: false, fallback_group_id: null, fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false, default_mapped_model: '', messages_dispatch_model_config: undefined,
  require_oauth_only: false, require_privacy_set: false, created_at: '', updated_at: '', model_routing: null,
  model_routing_enabled: false, mcp_xml_inject: true, supported_model_scopes: [], account_count: 1,
  active_account_count: 1, rate_limited_account_count: 0, model_allowlist: undefined, sort_order: 10,
}

const AppLayoutStub = defineComponent({ template: '<main><slot /></main>' })
const TablePageLayoutStub = defineComponent({ template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>' })
const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: '<div><div v-for="row in data" :key="row.id"><slot name="cell-actions" :row="row" /></div></div>',
})
const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})
const SelectStub = defineComponent({
  props: { modelValue: { type: [String, Number, Boolean], default: '' }, options: { type: Array, default: () => [] } },
  emits: ['update:modelValue', 'change'],
  setup(props, { attrs, emit }) {
    return () => h('select', { ...attrs, value: props.modelValue, onChange: (event: Event) => {
      const value = (event.target as HTMLSelectElement).value
      emit('update:modelValue', value)
      emit('change', value)
    } }, (props.options as Array<{ value: string; label: string }>).map(option => h('option', { value: option.value }, option.label)))
  },
})
const GroupI18nFieldStub = defineComponent({
  props: { modelValue: { type: String, default: '' }, i18n: { type: Object, default: () => ({}) } },
  emits: ['update:modelValue', 'update:i18n'],
  setup(props, { attrs, emit }) {
    return () => h('input', { ...attrs, value: props.modelValue, onInput: (event: Event) => emit('update:modelValue', (event.target as HTMLInputElement).value) })
  },
})

function mountView() {
  return mount(GroupsView, { global: { stubs: {
    AppLayout: AppLayoutStub, TablePageLayout: TablePageLayoutStub, DataTable: DataTableStub, Pagination: true,
    BaseDialog: BaseDialogStub, ConfirmDialog: true, EmptyState: true, Select: SelectStub, PlatformIcon: true,
    Icon: true, GroupCapacityBadge: true, GroupRateMultipliersModal: true, GroupRPMOverridesModal: true,
    VueDraggable: true, GroupI18nField: GroupI18nFieldStub,
  } } })
}

function checkboxByLabel(wrapper: ReturnType<typeof mountView>, suffix: string) {
  const label = wrapper.findAll('label').find(node => node.text().includes(suffix))
  expect(label, `label ${suffix}`).toBeTruthy()
  return label!.get('input[type="checkbox"]')
}

describe('GroupsView image_count_billing', () => {
  beforeEach(() => {
    authState.isSimpleMode = false
    localStorage.clear()
    listGroups.mockReset().mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
    createGroup.mockReset().mockResolvedValue({ ...group, id: 43 })
    updateGroup.mockReset().mockResolvedValue(group)
    duplicateGroup.mockReset()
    getModelAllowlistCandidates.mockReset().mockResolvedValue([])
    getUsageSummary.mockReset().mockResolvedValue([])
    getCapacitySummary.mockReset().mockResolvedValue([])
    getLiveCapability.mockReset().mockResolvedValue({ supported: false })
  })
  afterEach(() => vi.restoreAllMocks())

  it('creates and edits the image billing switch, and clears it for an unsupported platform', async () => {
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-tour="groups-create-btn"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-tour="group-form-name"]').setValue('Image group')
    await wrapper.get('[data-tour="group-form-platform"]').setValue('openai')
    await flushPromises()
    await checkboxByLabel(wrapper, 'admin.groups.imagePricing.allowImageGeneration').setValue(true)
    await checkboxByLabel(wrapper, 'admin.groups.imagePricing.perImageBilling').setValue(true)
    expect(wrapper.findAll('label').some(label => label.text().includes('admin.groups.imagePricing.perImageBilling'))).toBe(true)
    await wrapper.get('#create-group-form').trigger('submit')
    await flushPromises()
    expect(createGroup).toHaveBeenCalledWith(expect.objectContaining({
      platform: 'openai', allow_image_generation: true, image_count_billing: true,
    }))

    await wrapper.get('[data-tour="groups-create-btn"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-tour="group-form-name"]').setValue('Image group')
    await wrapper.get('[data-tour="group-form-platform"]').setValue('openai')
    await flushPromises()
    await checkboxByLabel(wrapper, 'admin.groups.imagePricing.allowImageGeneration').setValue(true)
    await checkboxByLabel(wrapper, 'admin.groups.imagePricing.perImageBilling').setValue(true)
    await wrapper.get('[data-tour="group-form-platform"]').setValue('anthropic')
    await flushPromises()
    expect(wrapper.findAll('label').some(label => label.text().includes('admin.groups.imagePricing.perImageBilling'))).toBe(false)
    await wrapper.get('#create-group-form').trigger('submit')
    await flushPromises()
    expect(createGroup).toHaveBeenLastCalledWith(expect.objectContaining({ platform: 'anthropic', image_count_billing: false }))

    listGroups.mockResolvedValueOnce({ items: [{ ...group, image_count_billing: true }], total: 1, page: 1, page_size: 20, pages: 1 })
    const editWrapper = mountView()
    await flushPromises()
    await editWrapper.findAll('button').find(button => button.text() === 'common.edit')!.trigger('click')
    await flushPromises()
    const editToggle = checkboxByLabel(editWrapper, 'admin.groups.imagePricing.perImageBilling')
    expect((editToggle.element as HTMLInputElement).checked).toBe(true)
    await editToggle.setValue(false)
    await editWrapper.get('#edit-group-form').trigger('submit')
    await flushPromises()
    expect(updateGroup).toHaveBeenCalledWith(42, expect.objectContaining({ platform: 'openai', image_count_billing: false }))
    wrapper.unmount()
    editWrapper.unmount()
  })

  it('keeps the image billing fields when a group is copied and edited', async () => {
    const copied = { ...group, id: 43, name: 'Image group (Copy)', image_count_billing: true }
    listGroups
      .mockResolvedValueOnce({ items: [group], total: 1, page: 1, page_size: 20, pages: 1 })
      .mockResolvedValueOnce({ items: [group, copied], total: 2, page: 1, page_size: 20, pages: 1 })
    duplicateGroup.mockResolvedValueOnce(copied)
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-testid="group-duplicate"]').trigger('click')
    await flushPromises()
    expect(duplicateGroup).toHaveBeenCalledWith(group.id)

    await wrapper.findAll('button').filter(button => button.text() === 'common.edit')[1].trigger('click')
    await flushPromises()
    const copiedToggle = checkboxByLabel(wrapper, 'admin.groups.imagePricing.perImageBilling')
    expect((copiedToggle.element as HTMLInputElement).checked).toBe(true)
    await copiedToggle.setValue(false)
    await wrapper.get('#edit-group-form').trigger('submit')
    await flushPromises()
    expect(updateGroup).toHaveBeenCalledWith(copied.id, expect.objectContaining({
      platform: 'openai', allow_image_generation: true, image_count_billing: false,
    }))
    wrapper.unmount()
  })
})
