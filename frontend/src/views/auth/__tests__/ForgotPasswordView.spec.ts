import { defineComponent, h } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ForgotPasswordView from '@/views/auth/ForgotPasswordView.vue'

const { getPublicSettingsMock, forgotPasswordMock, showErrorMock, showSuccessMock, captchaResetMock } = vi.hoisted(() => ({
  getPublicSettingsMock: vi.fn(),
  forgotPasswordMock: vi.fn(),
  showErrorMock: vi.fn(),
  showSuccessMock: vi.fn(),
  captchaResetMock: vi.fn()
}))

const CaptchaChallengeStub = defineComponent({
  name: 'TurnstileWidgetStub',
  emits: ['verify', 'expire', 'error'],
  setup(_, { expose }) {
    expose({ reset: captchaResetMock, verifyAction: vi.fn() })
    return () => h('div', { 'data-testid': 'turnstile-widget' })
  }
})

vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError: showErrorMock, showSuccess: showSuccessMock })
}))
vi.mock('@/api/auth', async () => {
  const actual = await vi.importActual<typeof import('@/api/auth')>('@/api/auth')
  return {
    ...actual,
    getPublicSettings: (...args: unknown[]) => getPublicSettingsMock(...args),
    forgotPassword: (...args: unknown[]) => forgotPasswordMock(...args)
  }
})

function mountForgotPassword() {
  return mount(ForgotPasswordView, {
    global: {
      stubs: {
        AuthLayout: { template: '<div><slot /><slot name="footer" /></div>' },
        Icon: true,
        TurnstileWidget: CaptchaChallengeStub,
        transition: false
      }
    }
  })
}

describe('ForgotPasswordView Turnstile integration', () => {
  beforeEach(() => {
    getPublicSettingsMock.mockReset()
    forgotPasswordMock.mockReset()
    showErrorMock.mockReset()
    showSuccessMock.mockReset()
    captchaResetMock.mockReset()
    getPublicSettingsMock.mockResolvedValue({ turnstile_enabled: true, turnstile_site_key: 'site-key' })
    forgotPasswordMock.mockResolvedValue(undefined)
  })

  it('blocks missing and expired proof, then submits the replacement token and resets afterward', async () => {
    const wrapper = mountForgotPassword()
    await flushPromises()
    await wrapper.get('#email').setValue('user@example.com')

    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(forgotPasswordMock).not.toHaveBeenCalled()

    const captcha = wrapper.getComponent(CaptchaChallengeStub)
    captcha.vm.$emit('verify', 'expired-proof', '')
    captcha.vm.$emit('expire')
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(forgotPasswordMock).not.toHaveBeenCalled()

    captcha.vm.$emit('verify', 'replacement-proof', '')
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(forgotPasswordMock).toHaveBeenCalledWith({
      email: 'user@example.com',
      turnstile_token: 'replacement-proof',
      tencent_captcha_ticket: undefined,
      tencent_captcha_randstr: undefined
    })
    expect(captchaResetMock).toHaveBeenCalledOnce()
    expect(wrapper.find('[data-testid="turnstile-widget"]').exists()).toBe(false)
  })

  it('keeps the closed Turnstile path free of a widget and proof fields', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({ turnstile_enabled: false, turnstile_site_key: '' })
    const wrapper = mountForgotPassword()
    await flushPromises()
    await wrapper.get('#email').setValue('user@example.com')
    expect(wrapper.find('[data-testid="turnstile-widget"]').exists()).toBe(false)

    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(forgotPasswordMock).toHaveBeenCalledWith({
      email: 'user@example.com',
      turnstile_token: undefined,
      tencent_captcha_ticket: undefined,
      tencent_captcha_randstr: undefined
    })
  })

  it('keeps missing proof blocked when Turnstile is enabled without a site key', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({ turnstile_enabled: true, turnstile_site_key: '' })
    const wrapper = mountForgotPassword()
    await flushPromises()
    await wrapper.get('#email').setValue('user@example.com')

    expect(wrapper.find('[data-testid="turnstile-widget"]').exists()).toBe(false)
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeDefined()
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()

    expect(forgotPasswordMock).not.toHaveBeenCalled()
  })
})
