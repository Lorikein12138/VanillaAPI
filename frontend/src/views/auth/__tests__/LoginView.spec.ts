import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import LoginView from '@/views/auth/LoginView.vue'

const { appStoreState, getPublicSettingsMock, pushMock, loginMock, showErrorMock, showSuccessMock } = vi.hoisted(() => ({
  appStoreState: { cachedPublicSettings: null as Record<string, unknown> | null },
  getPublicSettingsMock: vi.fn(),
  pushMock: vi.fn(),
  loginMock: vi.fn(),
  showErrorMock: vi.fn(),
  showSuccessMock: vi.fn()
}))

const CaptchaChallengeStub = defineComponent({
  name: 'TurnstileWidgetStub',
  emits: ['verify', 'expire', 'error'],
  setup(_, { expose }) {
    expose({ reset: vi.fn(), verifyAction: vi.fn() })
    return () => h('div', { 'data-testid': 'turnstile-widget' })
  }
})

const publicSettings = {
  registration_enabled: true,
  turnstile_enabled: false,
  turnstile_site_key: '',
  tencent_captcha_enabled: false,
  tencent_captcha_app_id: '',
  aliyun_captcha_enabled: false,
  aliyun_captcha_scene_id: '',
  aliyun_captcha_prefix: '',
  linuxdo_oauth_enabled: false,
  dingtalk_oauth_enabled: false,
  wechat_oauth_enabled: false,
  backend_mode_enabled: false,
  oidc_oauth_enabled: false,
  oidc_oauth_provider_name: 'OIDC',
  github_oauth_enabled: false,
  google_oauth_enabled: false,
  password_reset_enabled: false,
  passkey_enabled: false,
  login_agreement_enabled: false,
  login_agreement_documents: []
}

vi.mock('vue-router', () => ({
  useRouter: () => ({
    push: pushMock,
    currentRoute: { value: { query: {} } }
  })
}))

vi.mock('vue-i18n', () => ({
  createI18n: () => ({
    global: {
      t: (key: string) => key
    }
  }),
  useI18n: () => ({
    t: (key: string) => key
  })
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => ({
    login: (...args: unknown[]) => loginMock(...args),
    loginWithPasskey: vi.fn(),
    login2FA: vi.fn()
  }),
  useAppStore: () => ({
    cachedPublicSettings: appStoreState.cachedPublicSettings,
    showError: (...args: unknown[]) => showErrorMock(...args),
    showSuccess: (...args: unknown[]) => showSuccessMock(...args),
    showWarning: vi.fn()
  })
}))

vi.mock('@/api/auth', () => ({
  buildOAuthLoginStartURL: vi.fn(),
  getPublicSettings: (...args: unknown[]) => getPublicSettingsMock(...args),
  isTotp2FARequired: vi.fn(() => false),
  isWeChatWebOAuthEnabled: vi.fn(() => false),
  startOAuthLogin: vi.fn()
}))

function mountLogin() {
  return mount(LoginView, {
    global: {
      stubs: {
        AuthLayout: { template: '<div><slot /><slot name="footer" /></div>' },
        DingTalkOAuthSection: true,
        EmailOAuthButtons: true,
        Icon: true,
        LinuxDoOAuthSection: true,
        LoginAgreementPrompt: true,
        OidcOAuthSection: true,
        RouterLink: { template: '<a><slot /></a>' },
        TotpLoginModal: true,
        TurnstileWidget: CaptchaChallengeStub,
        WechatOAuthSection: true,
        transition: false
      }
    }
  })
}

describe('LoginView registration entry', () => {
  beforeEach(() => {
    appStoreState.cachedPublicSettings = null
    getPublicSettingsMock.mockReset()
    pushMock.mockReset()
    loginMock.mockReset()
    showErrorMock.mockReset()
    showSuccessMock.mockReset()
    getPublicSettingsMock.mockResolvedValue(publicSettings)
    loginMock.mockResolvedValue({})
  })

  it('shows the registration entry when registration is enabled', async () => {
    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.text()).toContain('auth.signUp')
  })

  it('hides the registration entry when registration is disabled', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({
      ...publicSettings,
      registration_enabled: false
    })

    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.text()).not.toContain('auth.signUp')
  })

  it('renders the registration entry from cached settings without refetching', async () => {
    appStoreState.cachedPublicSettings = publicSettings

    const wrapper = mountLogin()
    await wrapper.vm.$nextTick()

    expect(wrapper.text()).toContain('auth.signUp')
    expect(getPublicSettingsMock).not.toHaveBeenCalled()
  })

  it('blocks login after the captcha expires and sends only the replacement token', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({
      ...publicSettings,
      turnstile_enabled: true,
      turnstile_site_key: 'site-key'
    })
    const wrapper = mountLogin()
    await flushPromises()
    await wrapper.get('#email').setValue('user@example.com')
    await wrapper.get('#password').setValue('secret-123')

    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(loginMock).not.toHaveBeenCalled()

    const captcha = wrapper.getComponent(CaptchaChallengeStub)
    captcha.vm.$emit('verify', 'expired-proof', '')
    captcha.vm.$emit('expire')
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(loginMock).not.toHaveBeenCalled()

    captcha.vm.$emit('verify', 'replacement-proof', '')
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(loginMock).toHaveBeenCalledWith(expect.objectContaining({
      email: 'user@example.com',
      password: 'secret-123',
      turnstile_token: 'replacement-proof'
    }))
  })
})
