import { defineComponent, h } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import CaptchaChallenge from '../CaptchaChallenge.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const mounted: VueWrapper[] = []
const renderOptions: Array<{
  callback: (token: string) => void
  'expired-callback'?: () => void
  'error-callback'?: () => void
  size?: string
}> = []
let nextId = 0
let observerCallback: ResizeObserverCallback | undefined

class ChallengeResizeObserver {
  constructor(callback: ResizeObserverCallback) {
    observerCallback = callback
  }
  observe() {}
  disconnect() {}
}

const resetTencent = vi.fn()
const verifyTencent = vi.fn()
const resetAliyun = vi.fn()
const verifyAliyun = vi.fn()

const TencentCaptchaStub = defineComponent({
  name: 'TencentCaptchaGate',
  props: ['appId', 'region'],
  emits: ['verify', 'expire', 'error'],
  inheritAttrs: false,
  setup(_, { expose }) {
    expose({ reset: resetTencent, verify: verifyTencent })
    return () => h('div', { 'data-testid': 'tencent-captcha' })
  }
})

const AliyunCaptchaStub = defineComponent({
  name: 'AliyunCaptchaWidget',
  props: ['sceneId', 'prefix', 'region'],
  emits: ['verify', 'expire', 'error'],
  inheritAttrs: false,
  setup(_, { expose }) {
    expose({ reset: resetAliyun, verify: verifyAliyun })
    return () => h('div', { 'data-testid': 'aliyun-captcha' })
  }
})

function installTurnstile(width = 222) {
  nextId = 0
  renderOptions.length = 0
  observerCallback = undefined
  vi.stubGlobal('ResizeObserver', ChallengeResizeObserver)
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    if (!this.classList.contains('turnstile-wrapper')) return new DOMRect()
    return { width, height: 140, x: 0, y: 0, top: 0, left: 0, right: width, bottom: 140, toJSON: () => ({}) } as DOMRect
  })
  window.turnstile = {
    render: (_container, options) => {
      renderOptions.push(options)
      return `widget-${++nextId}`
    },
    reset: vi.fn(),
    remove: vi.fn()
  }
}

const baseProps = {
  turnstileEnabled: true,
  turnstileSiteKey: 'site-key',
  tencentEnabled: false,
  tencentAppId: ''
}

afterEach(() => {
  mounted.splice(0).forEach(wrapper => wrapper.unmount())
  delete window.turnstile
  delete window.onTurnstileLoad
  renderOptions.length = 0
  observerCallback = undefined
  resetTencent.mockReset()
  verifyTencent.mockReset()
  resetAliyun.mockReset()
  verifyAliyun.mockReset()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('CaptchaChallenge', () => {
  it('bridges Turnstile verify, expiry, errors, size rebuild and reset to the current id', async () => {
    installTurnstile()
    const wrapper = mount(CaptchaChallenge, { props: baseProps })
    mounted.push(wrapper)
    await flushPromises()

    expect(renderOptions[0]?.size).toBe('compact')
    renderOptions[0]!.callback('old-token')
    expect(wrapper.emitted('verify')).toEqual([['old-token', '']])
    expect(wrapper.emitted('expire')).toBeUndefined()

    const resizeEntry = {
      target: wrapper.get('.turnstile-wrapper').element,
      contentRect: { width: 300, height: 140, x: 0, y: 0, top: 0, right: 300, bottom: 140, left: 0, toJSON: () => ({}) }
    } as ResizeObserverEntry
    observerCallback?.([resizeEntry], {} as ResizeObserver)

    expect(wrapper.emitted('expire')).toEqual([[]])
    expect(renderOptions).toHaveLength(2)
    renderOptions[0]!['expired-callback']?.()
    expect(wrapper.emitted('expire')).toEqual([[]])
    renderOptions[0]!['error-callback']?.()
    renderOptions[1]!['expired-callback']?.()
    renderOptions[1]!['error-callback']?.()
    ;(wrapper.vm as unknown as { reset: () => void }).reset()

    expect(wrapper.emitted('expire')).toEqual([[], []])
    expect(wrapper.emitted('error')).toEqual([[]])
    expect(window.turnstile?.reset).toHaveBeenCalledWith('widget-2')
  })

  it('keeps the Tencent branch and exposes its action proof and reset', async () => {
    verifyTencent.mockResolvedValue({ ticket: 'ticket', randstr: 'randstr' })
    const warn = vi.spyOn(console, 'warn')
    try {
      const wrapper = mount(CaptchaChallenge, {
        props: {
          ...baseProps,
          turnstileEnabled: false,
          tencentEnabled: true,
          tencentAppId: 'tencent-app',
          tencentRegion: 'intl'
        },
        global: { stubs: { TencentCaptchaGate: TencentCaptchaStub } }
      })
      mounted.push(wrapper)

      expect(wrapper.get('[data-testid="tencent-captcha"]').exists()).toBe(true)
      expect(wrapper.getComponent(TencentCaptchaStub).props()).toMatchObject({ appId: 'tencent-app', region: 'intl' })
      expect(wrapper.find('.turnstile-wrapper').exists()).toBe(false)
      await expect((wrapper.vm as unknown as { verifyAction: () => Promise<unknown> }).verifyAction())
        .resolves.toEqual({ token: 'ticket', randstr: 'randstr' })
      ;(wrapper.vm as unknown as { reset: () => void }).reset()

      expect(verifyTencent).toHaveBeenCalledOnce()
      expect(resetTencent).toHaveBeenCalledOnce()
      expect(warn).not.toHaveBeenCalled()
    } finally {
      warn.mockRestore()
    }
  })

  it('keeps the Aliyun branch and forwards its proof and events', async () => {
    verifyAliyun.mockResolvedValue('aliyun-proof')
    const warn = vi.spyOn(console, 'warn')
    try {
      const wrapper = mount(CaptchaChallenge, {
        props: {
          ...baseProps,
          turnstileEnabled: false,
          turnstileSiteKey: '',
          tencentEnabled: false,
          aliyunEnabled: true,
          aliyunSceneId: 'scene',
          aliyunPrefix: 'prefix'
        },
        global: { stubs: { AliyunCaptchaWidget: AliyunCaptchaStub } }
      })
      mounted.push(wrapper)

      expect(wrapper.get('[data-testid="aliyun-captcha"]').exists()).toBe(true)
      expect(wrapper.getComponent(AliyunCaptchaStub).props()).toMatchObject({ sceneId: 'scene', prefix: 'prefix', region: 'cn' })
      await expect((wrapper.vm as unknown as { verifyAction: () => Promise<unknown> }).verifyAction())
        .resolves.toEqual({ token: 'aliyun-proof', randstr: '' })
      wrapper.getComponent(AliyunCaptchaStub).vm.$emit('verify', 'direct-proof')
      wrapper.getComponent(AliyunCaptchaStub).vm.$emit('expire')
      wrapper.getComponent(AliyunCaptchaStub).vm.$emit('error')
      ;(wrapper.vm as unknown as { reset: () => void }).reset()

      expect(wrapper.emitted('verify')).toEqual([['direct-proof', '']])
      expect(wrapper.emitted('expire')).toEqual([[]])
      expect(wrapper.emitted('error')).toEqual([[]])
      expect(resetAliyun).toHaveBeenCalledOnce()
      expect(warn).not.toHaveBeenCalled()
    } finally {
      warn.mockRestore()
    }
  })
})
