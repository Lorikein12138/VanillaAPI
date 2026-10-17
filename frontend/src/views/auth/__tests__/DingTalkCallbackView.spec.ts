import { defineComponent, h, nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import DingTalkCallbackView from '../DingTalkCallbackView.vue'

const { exchangePendingOAuthCompletionMock, getPublicSettingsMock, apiClientPostMock, setPendingAuthSessionMock } = vi.hoisted(() => ({
  exchangePendingOAuthCompletionMock: vi.fn(),
  getPublicSettingsMock: vi.fn(),
  apiClientPostMock: vi.fn(),
  setPendingAuthSessionMock: vi.fn()
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
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key, te: () => false }) }
})
vi.mock('@/stores', () => ({
  useAuthStore: () => ({ setPendingAuthSession: setPendingAuthSessionMock, setToken: vi.fn(), clearPendingAuthSession: vi.fn() }),
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/api/client', () => ({ apiClient: { post: (...args: unknown[]) => apiClientPostMock(...args) } }))
vi.mock('@/api/auth', async () => {
  const actual = await vi.importActual<typeof import('@/api/auth')>('@/api/auth')
  return {
    ...actual,
    exchangePendingOAuthCompletion: (...args: unknown[]) => exchangePendingOAuthCompletionMock(...args),
    getPublicSettings: (...args: unknown[]) => getPublicSettingsMock(...args)
  }
})

function mountCallback() {
  return mount(DingTalkCallbackView, {
    global: {
      stubs: {
        AuthLayout: { template: '<div><slot /></div>' },
        Icon: true,
        RouterLink: { template: '<a><slot /></a>' },
        TurnstileWidget: CaptchaChallengeStub,
        transition: false
      }
    }
  })
}

describe('DingTalkCallbackView Turnstile integration', () => {
  beforeEach(() => {
    exchangePendingOAuthCompletionMock.mockReset()
    getPublicSettingsMock.mockReset()
    apiClientPostMock.mockReset()
    setPendingAuthSessionMock.mockReset()
    routeState.query = {}
    window.location.hash = ''
    localStorage.clear()
    sessionStorage.clear()
    getPublicSettingsMock.mockResolvedValue({ turnstile_enabled: true, turnstile_site_key: 'site-key' })
    exchangePendingOAuthCompletionMock.mockResolvedValue({
      step: 'create_account_required',
      pending_email: 'prefilled@example.com',
      redirect: '/welcome'
    })
    apiClientPostMock.mockResolvedValue({ data: { access_token: 'access', refresh_token: 'refresh', expires_in: 3600, token_type: 'Bearer' } })
  })

  it('uses the real create-account form gate after expiry and submits the replacement token', async () => {
    const wrapper = mountCallback()
    await flushPromises()
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
    expect(apiClientPostMock).toHaveBeenCalledWith('/auth/oauth/pending/create-account', expect.objectContaining({
      email: 'prefilled@example.com',
      password: 'secret-123',
      verify_code: '246810',
      turnstile_token: 'replacement-proof'
    }))
    expect(setPendingAuthSessionMock).toHaveBeenCalled()
  })

  it('keeps the create-account form free of a Turnstile placeholder when disabled', async () => {
    getPublicSettingsMock.mockResolvedValue({ turnstile_enabled: false, turnstile_site_key: '' })
    const wrapper = mountCallback()
    await flushPromises()
    await wrapper.get('[data-testid="dingtalk-create-account-password"]').setValue('secret-123')
    await wrapper.get('[data-testid="dingtalk-create-account-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="turnstile-stub"]').exists()).toBe(false)
    expect(apiClientPostMock).toHaveBeenCalledWith('/auth/oauth/pending/create-account', expect.not.objectContaining({ turnstile_token: expect.anything() }))
  })

  it('does not show the shared create-account form in the bind-login branch', async () => {
    exchangePendingOAuthCompletionMock.mockResolvedValueOnce({
      step: 'bind_login_required',
      existing_account_email: 'existing@example.com',
      redirect: '/welcome'
    })
    const wrapper = mountCallback()
    await flushPromises()

    expect(wrapper.find('[data-testid="dingtalk-create-account-submit"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="turnstile-stub"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="dingtalk-bind-login-email"]').element).toBeTruthy()
  })
})
