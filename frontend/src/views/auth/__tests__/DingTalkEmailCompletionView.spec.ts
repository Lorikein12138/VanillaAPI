import { defineComponent, h, nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import DingTalkEmailCompletionView from '../DingTalkEmailCompletionView.vue'

const { getPublicSettingsMock, apiClientPostMock, setTokenMock, showSuccessMock, replaceMock } = vi.hoisted(() => ({
  getPublicSettingsMock: vi.fn(),
  apiClientPostMock: vi.fn(),
  setTokenMock: vi.fn(),
  showSuccessMock: vi.fn(),
  replaceMock: vi.fn()
}))

const routeState = { query: {} as Record<string, unknown> }
const CaptchaChallengeStub = defineComponent({
  name: 'TurnstileWidgetStub',
  emits: ['verify', 'expire', 'error'],
  setup(_, { expose }) {
    expose({ reset: vi.fn(), verifyAction: vi.fn() })
    return () => h('div', { 'data-testid': 'turnstile-stub' })
  }
})

vi.mock('vue-router', () => ({
  useRoute: () => routeState,
  useRouter: () => ({ replace: replaceMock, push: vi.fn() })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/stores', () => ({
  useAuthStore: () => ({ setToken: (...args: unknown[]) => setTokenMock(...args) }),
  useAppStore: () => ({ showSuccess: (...args: unknown[]) => showSuccessMock(...args), showInfo: vi.fn(), showError: vi.fn() })
}))
vi.mock('@/api/client', () => ({ apiClient: { post: (...args: unknown[]) => apiClientPostMock(...args) } }))
vi.mock('@/api/auth', async () => {
  const actual = await vi.importActual<typeof import('@/api/auth')>('@/api/auth')
  return { ...actual, getPublicSettings: (...args: unknown[]) => getPublicSettingsMock(...args) }
})

function mountEmailCompletion() {
  return mount(DingTalkEmailCompletionView, {
    global: {
      stubs: {
        AuthLayout: { template: '<div><slot /></div>' },
        Icon: true,
        TurnstileWidget: CaptchaChallengeStub
      }
    }
  })
}

describe('DingTalkEmailCompletionView Turnstile integration', () => {
  beforeEach(() => {
    getPublicSettingsMock.mockReset()
    apiClientPostMock.mockReset()
    setTokenMock.mockReset()
    showSuccessMock.mockReset()
    replaceMock.mockReset()
    routeState.query = { email: 'prefilled@example.com', redirect: '/billing' }
    localStorage.clear()
    sessionStorage.clear()
    getPublicSettingsMock.mockResolvedValue({ email_verify_enabled: true, turnstile_enabled: true, turnstile_site_key: 'site-key' })
    apiClientPostMock.mockResolvedValue({ data: { access_token: 'access-token', redirect: '/billing' } })
    setTokenMock.mockResolvedValue({})
  })

  it('keeps the email context and blocks expired proof before posting the replacement token', async () => {
    const wrapper = mountEmailCompletion()
    await flushPromises()
    expect((wrapper.get('[data-testid="dingtalk-create-account-email"]').element as HTMLInputElement).value)
      .toBe('prefilled@example.com')
    expect(wrapper.find('[data-testid="oidc-create-account-email"]').exists()).toBe(false)
    await wrapper.get('[data-testid="dingtalk-create-account-password"]').setValue('secret-123')
    await wrapper.get('[data-testid="dingtalk-create-account-verify-code"]').setValue('246810')

    await wrapper.get('[data-testid="dingtalk-create-account-submit"]').trigger('click')
    await flushPromises()
    expect(apiClientPostMock).not.toHaveBeenCalled()

    const captcha = wrapper.getComponent(CaptchaChallengeStub)
    captcha.vm.$emit('verify', 'expired-proof', '')
    await nextTick()
    expect(wrapper.get('[data-testid="dingtalk-create-account-submit"]').attributes('disabled')).toBeUndefined()
    captcha.vm.$emit('expire')
    await nextTick()
    expect(wrapper.get('[data-testid="dingtalk-create-account-submit"]').attributes('disabled')).toBeDefined()
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(apiClientPostMock).not.toHaveBeenCalled()

    captcha.vm.$emit('verify', 'replacement-proof', '')
    await nextTick()
    expect(wrapper.get('[data-testid="dingtalk-create-account-submit"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('[data-testid="dingtalk-create-account-submit"]').trigger('click')
    await flushPromises()

    expect(apiClientPostMock).toHaveBeenCalledWith('/auth/oauth/pending/create-account', {
      email: 'prefilled@example.com',
      password: 'secret-123',
      verify_code: '246810',
      turnstile_token: 'replacement-proof',
      invitation_code: undefined
    })
    expect(setTokenMock).toHaveBeenCalledWith('access-token')
    expect(replaceMock).toHaveBeenCalledWith('/billing')
  })

  it('keeps the form free of a Turnstile placeholder and token when disabled', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({ email_verify_enabled: false, turnstile_enabled: false, turnstile_site_key: '' })
    apiClientPostMock.mockResolvedValueOnce({ data: { step: 'choose_account_action_required', existing_account_bindable: true } })
    const wrapper = mountEmailCompletion()
    await flushPromises()
    await wrapper.get('[data-testid="dingtalk-create-account-password"]').setValue('secret-123')
    expect(wrapper.find('[data-testid="turnstile-stub"]').exists()).toBe(false)

    await wrapper.get('[data-testid="dingtalk-create-account-submit"]').trigger('click')
    await flushPromises()

    expect(apiClientPostMock).toHaveBeenCalledWith('/auth/oauth/pending/create-account', expect.not.objectContaining({ turnstile_token: expect.anything() }))
  })
})
