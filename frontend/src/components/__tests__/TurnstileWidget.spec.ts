import { nextTick } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import TurnstileWidget from '../TurnstileWidget.vue'
import widgetSource from '../TurnstileWidget.vue?raw'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const scriptSelector = 'script[src*="challenges.cloudflare.com/turnstile"]'
const wrappers: VueWrapper[] = []
let wrapperWidth = 382

type RenderOptions = {
  sitekey: string
  callback: (token: string) => void
  'expired-callback'?: () => void
  'error-callback'?: () => void
  theme?: 'light' | 'dark' | 'auto'
  size?: 'normal' | 'compact' | 'flexible'
}

class ControlledResizeObserver {
  static instances: ControlledResizeObserver[] = []
  readonly disconnect = vi.fn()
  private target: Element | null = null
  readonly observe = vi.fn((target: Element) => {
    this.target = target
  })

  constructor(private readonly callback: ResizeObserverCallback) {
    ControlledResizeObserver.instances.push(this)
  }

  notify(width: number) {
    if (!this.target) throw new Error('ResizeObserver target was not registered')
    const entry = {
      target: this.target,
      contentRect: { width, height: 140, x: 0, y: 0, top: 0, right: width, bottom: 140, left: 0, toJSON: () => ({}) }
    } as ResizeObserverEntry
    this.callback([entry], this as unknown as ResizeObserver)
  }
}

function installGeometry(width = 382, observer = true) {
  wrapperWidth = width
  ControlledResizeObserver.instances = []
  if (observer) vi.stubGlobal('ResizeObserver', ControlledResizeObserver)
  else vi.stubGlobal('ResizeObserver', undefined)

  const original = HTMLElement.prototype.getBoundingClientRect
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    if (!this.classList.contains('turnstile-wrapper')) return original.call(this)
    const value = wrapperWidth
    return {
      x: 0,
      y: 0,
      top: 0,
      left: 0,
      right: value,
      bottom: 140,
      width: value,
      height: 140,
      toJSON: () => ({})
    } as DOMRect
  })
}

function installSDK(options: { throwOnRender?: boolean } = {}) {
  const renders: Array<{ container: HTMLElement; options: RenderOptions }> = []
  const render = vi.fn((container: HTMLElement, renderOptions: RenderOptions) => {
    if (options.throwOnRender) throw new Error('render failed')
    renders.push({ container, options: renderOptions })
    return `widget-${renders.length}`
  })
  const reset = vi.fn()
  const remove = vi.fn()
  window.turnstile = { render, reset, remove }
  return { render, renders, reset, remove }
}

function mountWidget(props: { siteKey?: string; size?: 'normal' | 'compact' | 'flexible' } = {}) {
  const mounted = mount(TurnstileWidget, { props: { siteKey: 'site-key', ...props } })
  wrappers.push(mounted)
  return mounted
}

function unmountWidget(mounted: VueWrapper) {
  mounted.unmount()
  const index = wrappers.indexOf(mounted)
  if (index >= 0) wrappers.splice(index, 1)
}

afterEach(() => {
  wrappers.splice(0).forEach(item => item.unmount())
  delete window.turnstile
  delete window.onTurnstileLoad
  document.querySelectorAll(scriptSelector).forEach(script => script.remove())
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('TurnstileWidget', () => {
  it('shows a loading placeholder until the SDK initializes the widget', async () => {
    installGeometry(382)
    const mounted = mountWidget()
    await nextTick()

    expect(mounted.get('[role="status"]').text()).toBe('auth.captchaLoading')
    expect(mounted.get('.turnstile-stage').classes()).toContain('turnstile-stage--normal')
    expect(mounted.get('[role="status"]').element.parentElement).toBe(mounted.get('.turnstile-stage').element)
    expect(document.querySelector(scriptSelector)).not.toBeNull()
    const container = mounted.get('.turnstile-container').element

    const sdk = installSDK()
    window.onTurnstileLoad?.()
    await flushPromises()

    expect(sdk.render).toHaveBeenCalledWith(container, expect.objectContaining({ sitekey: 'site-key', size: 'normal' }))
    expect(mounted.find('[role="status"]').exists()).toBe(false)
    expect(mounted.get('.turnstile-container').element).toBe(container)
  })

  it.each([
    [0, 'normal', null],
    [222, 'normal', 'compact'],
    [277, 'normal', 'compact'],
    [299, 'normal', 'compact'],
    [300, 'normal', 'normal'],
    [382, 'normal', 'normal'],
    [222, 'compact', 'compact'],
    [277, 'compact', 'compact'],
    [299, 'compact', 'compact'],
    [300, 'compact', 'compact'],
    [382, 'compact', 'compact'],
    [222, 'flexible', 'compact'],
    [277, 'flexible', 'compact'],
    [299, 'flexible', 'compact'],
    [300, 'flexible', 'flexible'],
    [382, 'flexible', 'flexible']
  ] as const)('selects the SDK size for width %i and preferred %s', async (width, preferred, expected) => {
    installGeometry(width)
    const sdk = installSDK()
    const mounted = mountWidget({ size: preferred })
    await flushPromises()

    if (expected === null) {
      expect(sdk.render).not.toHaveBeenCalled()
      expect(mounted.get('.turnstile-stage').classes()).toContain('turnstile-stage--unmeasured')
      expect(mounted.find('[role="status"]').exists()).toBe(true)
      return
    }

    expect(sdk.render).toHaveBeenCalledOnce()
    expect(sdk.renders[0]?.options.size).toBe(expected)
    expect(mounted.get('.turnstile-stage').classes()).toContain(`turnstile-stage--${expected}`)
    expect(mounted.find('[role="status"]').exists()).toBe(false)
  })

  it('uses flexible width only when explicitly preferred and the host fits the normal widget', async () => {
    installGeometry(382)
    const sdk = installSDK()
    const mounted = mountWidget({ size: 'flexible' })
    await flushPromises()

    expect(sdk.renders[0]?.options.size).toBe('flexible')
    expect(mounted.get('.turnstile-stage').classes()).toContain('turnstile-stage--flexible')
  })

  it('does not override the SDK iframe width and keeps all loading UI inside the stage', async () => {
    installGeometry(222)
    const sdk = installSDK()
    const mounted = mountWidget()
    await flushPromises()
    sdk.renders[0]!.container.innerHTML = '<iframe></iframe>'
    await nextTick()

    expect(mounted.get('.turnstile-stage').classes()).toContain('turnstile-stage--compact')
    expect(mounted.get('iframe').attributes('style')).toBeUndefined()
    expect(widgetSource).not.toContain(':deep(iframe)')
    expect(widgetSource).not.toMatch(/(?:zoom|transform)\s*:/)
  })

  it('forwards verification from an already loaded SDK and resets the current widget id', async () => {
    installGeometry(382)
    const sdk = installSDK()
    const mounted = mountWidget()
    await flushPromises()

    expect(sdk.render).toHaveBeenCalledOnce()
    expect(document.querySelector(scriptSelector)).toBeNull()
    sdk.renders[0]!.options.callback('verified-token')
    ;(mounted.vm as unknown as { reset: () => void }).reset()

    expect(mounted.emitted('verify')).toEqual([['verified-token']])
    expect(sdk.reset).toHaveBeenCalledWith('widget-1')
  })

  it('does not churn an instance or expire its proof for repeated widths in the same size band', async () => {
    installGeometry(222)
    const sdk = installSDK()
    const mounted = mountWidget()
    await flushPromises()
    const observer = ControlledResizeObserver.instances[0]!

    observer.notify(277)
    observer.notify(299)

    expect(sdk.render).toHaveBeenCalledOnce()
    expect(sdk.remove).not.toHaveBeenCalled()
    expect(mounted.emitted('expire')).toBeUndefined()
  })

  it('rebuilds when the explicit preferred size changes', async () => {
    installGeometry(382)
    const sdk = installSDK()
    const mounted = mountWidget()
    await flushPromises()

    await mounted.setProps({ size: 'compact' })

    expect(sdk.render).toHaveBeenCalledTimes(2)
    expect(sdk.remove).toHaveBeenCalledWith('widget-1')
    expect(mounted.emitted('expire')).toEqual([[]])
    expect(sdk.renders[1]?.options.size).toBe('compact')
  })

  it('expires before removing once when crossing an effective size boundary', async () => {
    installGeometry(222)
    const events: string[] = []
    const sdk = installSDK()
    sdk.remove.mockImplementation(id => events.push(`remove:${id}`))
    const mounted = mount(TurnstileWidget, {
      props: { siteKey: 'site-key' },
      attrs: { onExpire: () => events.push('expire') }
    })
    wrappers.push(mounted)
    await flushPromises()
    const observer = ControlledResizeObserver.instances[0]!

    observer.notify(300)
    observer.notify(382)
    await nextTick()

    expect(sdk.render).toHaveBeenCalledTimes(2)
    expect(sdk.remove).toHaveBeenCalledTimes(1)
    expect(events).toEqual(['expire', 'remove:widget-1'])
    expect(mounted.get('.turnstile-stage').classes()).toContain('turnstile-stage--normal')
  })

  it('ignores callbacks from a removed generation and accepts callbacks from the current one', async () => {
    installGeometry(222)
    const sdk = installSDK()
    const mounted = mountWidget()
    await flushPromises()
    const stale = sdk.renders[0]!.options

    ControlledResizeObserver.instances[0]!.notify(300)
    const current = sdk.renders[1]!.options
    stale.callback('stale-token')
    stale['expired-callback']?.()
    stale['error-callback']?.()
    current.callback('current-token')

    expect(mounted.emitted('verify')).toEqual([['current-token']])
    expect(mounted.emitted('expire')).toEqual([[]])
    expect(mounted.emitted('error')).toBeUndefined()
  })

  it('keeps an instance while hidden at width zero and reconciles it when width returns', async () => {
    installGeometry(222)
    const sdk = installSDK()
    mountWidget()
    await flushPromises()
    const observer = ControlledResizeObserver.instances[0]!

    observer.notify(0)
    expect(sdk.render).toHaveBeenCalledOnce()
    expect(sdk.remove).not.toHaveBeenCalled()
    observer.notify(277)

    expect(sdk.render).toHaveBeenCalledOnce()
    expect(sdk.remove).not.toHaveBeenCalled()
  })

  it('uses and removes a window resize fallback when ResizeObserver is unavailable', async () => {
    installGeometry(222, false)
    const add = vi.spyOn(window, 'addEventListener')
    const remove = vi.spyOn(window, 'removeEventListener')
    const sdk = installSDK()
    const mounted = mountWidget()
    await flushPromises()

    expect(sdk.renders[0]?.options.size).toBe('compact')
    expect(add).toHaveBeenCalledWith('resize', expect.any(Function))
    wrapperWidth = 300
    window.dispatchEvent(new Event('resize'))
    expect(sdk.render).toHaveBeenCalledTimes(2)
    expect(sdk.renders[1]?.options.size).toBe('normal')

    unmountWidget(mounted)
    expect(remove).toHaveBeenCalledWith('resize', expect.any(Function))
  })

  it('ends loading and reports both script and render failures without throwing', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    installGeometry(382)
    const mounted = mountWidget()
    document.querySelector(scriptSelector)!.dispatchEvent(new Event('error'))
    await flushPromises()

    expect(mounted.find('[role="status"]').exists()).toBe(false)
    expect(mounted.emitted('error')).toEqual([[]])

    unmountWidget(mounted)
    installGeometry(382)
    const sdk = installSDK({ throwOnRender: true })
    const renderFailure = mountWidget()
    await flushPromises()

    expect(sdk.render).toHaveBeenCalledOnce()
    expect(renderFailure.find('[role="status"]').exists()).toBe(false)
    expect(renderFailure.emitted('error')).toEqual([[]])
  })

  it('does not render after unmount while the SDK load is pending', async () => {
    installGeometry(382)
    const mounted = mountWidget()
    expect(document.querySelector(scriptSelector)).not.toBeNull()
    unmountWidget(mounted)

    const sdk = installSDK()
    window.onTurnstileLoad?.()
    await flushPromises()

    expect(sdk.render).not.toHaveBeenCalled()
    expect(sdk.remove).not.toHaveBeenCalled()
  })

  it('keeps two widget instances and their callbacks isolated', async () => {
    installGeometry(382)
    const sdk = installSDK()
    const firstVerify = vi.fn()
    const firstExpire = vi.fn()
    const firstError = vi.fn()
    const secondVerify = vi.fn()
    const secondExpire = vi.fn()
    const secondError = vi.fn()
    const first = mount(TurnstileWidget, {
      props: { siteKey: 'site-key' },
      attrs: { onVerify: firstVerify, onExpire: firstExpire, onError: firstError }
    })
    const second = mount(TurnstileWidget, {
      props: { siteKey: 'site-key' },
      attrs: { onVerify: secondVerify, onExpire: secondExpire, onError: secondError }
    })
    wrappers.push(first, second)
    await flushPromises()

    expect(sdk.render).toHaveBeenCalledTimes(2)
    const firstOptions = sdk.renders[0]!.options
    const secondOptions = sdk.renders[1]!.options
    expect(firstOptions.callback).toBeTypeOf('function')
    expect(firstOptions['expired-callback']).toBeTypeOf('function')
    expect(firstOptions['error-callback']).toBeTypeOf('function')
    expect(secondOptions.callback).toBeTypeOf('function')
    expect(secondOptions['expired-callback']).toBeTypeOf('function')
    expect(secondOptions['error-callback']).toBeTypeOf('function')

    firstOptions.callback('first-token')
    const firstExpiredCallback = firstOptions['expired-callback'] as () => void
    firstExpiredCallback()
    const firstErrorCallback = firstOptions['error-callback'] as () => void
    firstErrorCallback()
    secondOptions.callback('second-token')
    const secondExpiredCallback = secondOptions['expired-callback'] as () => void
    secondExpiredCallback()
    const secondErrorCallback = secondOptions['error-callback'] as () => void
    secondErrorCallback()
    expect(firstVerify).toHaveBeenCalledWith('first-token')
    expect(firstExpire).toHaveBeenCalledOnce()
    expect(firstError).toHaveBeenCalledOnce()
    expect(secondVerify).toHaveBeenCalledWith('second-token')
    expect(secondExpire).toHaveBeenCalledOnce()
    expect(secondError).toHaveBeenCalledOnce()

    unmountWidget(first)
    firstOptions.callback('late-after-unmount')
    firstExpiredCallback()
    firstErrorCallback()
    secondOptions.callback('second-token-after-first-unmount')

    expect(firstVerify).toHaveBeenCalledTimes(1)
    expect(firstExpire).toHaveBeenCalledTimes(1)
    expect(firstError).toHaveBeenCalledTimes(1)
    expect(secondVerify.mock.calls).toEqual([['second-token'], ['second-token-after-first-unmount']])
    expect(secondExpire).toHaveBeenCalledOnce()
    expect(secondError).toHaveBeenCalledOnce()
    expect(sdk.remove).toHaveBeenCalledWith('widget-1')
    expect(sdk.remove).not.toHaveBeenCalledWith('widget-2')
  })

  it('does not show a placeholder or create an observer without a site key', async () => {
    installGeometry(382)
    const sdk = installSDK()
    const mounted = mountWidget({ siteKey: '' })
    await flushPromises()

    expect(mounted.find('.turnstile-wrapper').exists()).toBe(false)
    expect(mounted.find('[role="status"]').exists()).toBe(false)
    expect(sdk.render).not.toHaveBeenCalled()
    expect(ControlledResizeObserver.instances).toHaveLength(0)
    expect(document.querySelector(scriptSelector)).toBeNull()
  })
})
