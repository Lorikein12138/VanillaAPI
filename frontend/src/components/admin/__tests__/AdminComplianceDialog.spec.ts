import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AdminComplianceDialog from '../AdminComplianceDialog.vue'

const state = vi.hoisted(() => ({
  locale: 'zh',
  complianceStore: {
    status: { version: 'v2026.06.10', document_url_zh: '', document_url_en: '' },
    expectedPhrase: '我已阅读、理解并同意 Sub2API 部署与运营合规承诺',
    submitting: false,
    shouldShow: true,
    accept: vi.fn().mockResolvedValue({ required: false })
  },
  authStore: { isAuthenticated: true, isAdmin: true, logout: vi.fn() },
  appStore: { showSuccess: vi.fn(), showError: vi.fn() }
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/i18n', () => ({ getLocale: () => state.locale }))
vi.mock('@/stores', () => ({
  useAdminComplianceStore: () => state.complianceStore,
  useAuthStore: () => state.authStore,
  useAppStore: () => state.appStore
}))

afterEach(() => vi.clearAllMocks())

function mountDialog() {
  return mount(AdminComplianceDialog, {
    global: {
      stubs: {
        BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
        Input: {
          props: ['modelValue', 'disabled', 'error'],
          emits: ['update:modelValue', 'enter'],
          template: '<input v-bind="$attrs" :value="modelValue" :disabled="disabled" @input="$emit(\'update:modelValue\', $event.target.value)" />'
        },
        Icon: { template: '<span />' }
      }
    }
  })
}

describe('AdminComplianceDialog local agreement and confirmation flow', () => {
  it.each([
    ['zh', '部署与运营合规承诺', '任何第三方实例'],
    ['en', 'Deployment and Operation Compliance Commitment', 'Any third-party instance']
  ])('renders the %s agreement text', async (locale, title, body) => {
    state.locale = locale
    const wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.get('.legal-document-content').text()).toContain(title)
    expect(wrapper.get('.legal-document-content').text()).toContain(body)
  })

  it('keeps exact-phrase confirmation and submits the accepted phrase', async () => {
    state.locale = 'zh'
    state.complianceStore.expectedPhrase = '我已阅读、理解并同意 Sub2API 部署与运营合规承诺'
    state.complianceStore.accept.mockResolvedValue({ required: false })
    const wrapper = mountDialog()
    const acceptButton = wrapper.get('button.btn-primary')

    expect((acceptButton.element as HTMLButtonElement).disabled).toBe(true)
    await wrapper.get('#admin-compliance-phrase').setValue(state.complianceStore.expectedPhrase)
    expect((acceptButton.element as HTMLButtonElement).disabled).toBe(false)
    await acceptButton.trigger('click')
    await flushPromises()

    expect(state.complianceStore.accept).toHaveBeenCalledWith(state.complianceStore.expectedPhrase)
    expect(state.appStore.showSuccess).toHaveBeenCalledWith('adminCompliance.accepted')
  })
})
