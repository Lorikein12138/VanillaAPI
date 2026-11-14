import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { isMobileDevice } from '@/utils/device'

enableAutoUnmount(afterEach)
vi.mock('@/utils/device', () => ({ isMobileDevice: vi.fn(() => false) }))

const pollOrderStatus = vi.hoisted(() => vi.fn())
const cancelOrder = vi.hoisted(() => vi.fn())
const verifyOrder = vi.hoisted(() => vi.fn())
const showError = vi.hoisted(() => vi.fn())
const toCanvas = vi.hoisted(() => vi.fn())

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

vi.mock('@/stores/payment', () => ({
  usePaymentStore: () => ({
    pollOrderStatus,
  }),
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError,
  }),
}))

vi.mock('@/api/payment', () => ({
  paymentAPI: {
    cancelOrder,
    verifyOrder,
  },
}))

vi.mock('qrcode', () => ({
  default: {
    toCanvas,
  },
}))

import PaymentStatusPanel from '../PaymentStatusPanel.vue'

const orderFactory = (status: string) => ({
  id: 42,
  user_id: 9,
  amount: 88,
  pay_amount: 88,
  fee_rate: 0,
  payment_type: 'alipay',
  out_trade_no: 'sub2_20260420abcd1234',
  status,
  order_type: 'balance',
  created_at: '2026-04-20T12:00:00Z',
  expires_at: '2099-01-01T12:30:00Z',
  refund_amount: 0,
})

describe('PaymentStatusPanel', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    pollOrderStatus.mockReset()
    cancelOrder.mockReset()
    verifyOrder.mockReset()
    showError.mockReset()
    toCanvas.mockReset().mockResolvedValue(undefined)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('treats RECHARGING as a successful terminal state', async () => {
    pollOrderStatus.mockResolvedValue(orderFactory('RECHARGING'))

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(pollOrderStatus).toHaveBeenCalledWith(42)
    expect(wrapper.text()).toContain('payment.result.success')
    expect(wrapper.emitted('success')).toHaveLength(1)
  })

  it('shows reopen button in QR mode when payUrl is also available', async () => {
    const openSpy = vi.spyOn(window, 'open').mockReturnValue({ closed: false } as Window)

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        payUrl: 'https://pay.example.com/session/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')

    await wrapper.get('button.btn.btn-secondary.text-sm').trigger('click')
    expect(openSpy).toHaveBeenCalledWith(
      'https://pay.example.com/session/42',
      'paymentPopup',
      expect.any(String),
    )

    openSpy.mockRestore()
  })

  it('uses generic QR copy for custom methods that contain built-in names', async () => {
    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'card_alipay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('payment.qr.scanToPay')
    expect(wrapper.text()).not.toContain('payment.qr.scanAlipay')
  })

  it('actively verifies a stuck pending order and settles it when upstream confirms payment', async () => {
    pollOrderStatus.mockResolvedValue(orderFactory('PENDING'))
    verifyOrder.mockResolvedValue({
      data: orderFactory('COMPLETED'),
    })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'wxpay',
        orderType: 'balance',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(pollOrderStatus).toHaveBeenCalledWith(42)
    expect(verifyOrder).toHaveBeenCalledWith('sub2_20260420abcd1234')
    expect(wrapper.text()).toContain('payment.result.success')
    expect(wrapper.emitted('success')).toHaveLength(1)
  })

  it('actively verifies a pending mobile Alipay precreate order', async () => {
    const originalLocation = window.location
    const originalHidden = Object.getOwnPropertyDescriptor(document, 'hidden')
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { assign: vi.fn() },
    })
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => false,
    })
    pollOrderStatus.mockResolvedValue(orderFactory('PENDING'))
    verifyOrder.mockResolvedValue({ data: orderFactory('COMPLETED') })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/dynamic-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
        mobileAlipayDeepLink: true,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(verifyOrder).toHaveBeenCalledWith('sub2_20260420abcd1234')
    expect(wrapper.emitted('success')).toHaveLength(1)

    wrapper.unmount()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    if (originalHidden) Object.defineProperty(document, 'hidden', originalHidden)
  })

  it('actively verifies a pending desktop Alipay order', async () => {
    pollOrderStatus.mockResolvedValue(orderFactory('PENDING'))
    verifyOrder.mockResolvedValue({ data: orderFactory('COMPLETED') })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/desktop-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(verifyOrder).toHaveBeenCalledWith('sub2_20260420abcd1234')
    expect(wrapper.emitted('success')).toHaveLength(1)

    wrapper.unmount()
  })

  it('keeps the QR fallback hidden until the Alipay app launch times out', async () => {
    const originalLocation = window.location
    const originalHidden = Object.getOwnPropertyDescriptor(document, 'hidden')
    const assign = vi.fn()
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { assign },
    })
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => false,
    })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/dynamic-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
        mobileAlipayDeepLink: true,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    expect(assign).toHaveBeenCalledWith(expect.stringContaining('alipays://platformapi/startapp?saId=10000007&qrcode='))
    expect(wrapper.find('[data-test="alipay-qr-fallback"]').exists()).toBe(false)

    await vi.advanceTimersByTimeAsync(2200)
    await flushPromises()

    expect(wrapper.find('[data-test="alipay-qr-fallback"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('payment.qr.saveQRCode')
    expect(wrapper.text()).toContain('sub2_20260420abcd1234')
    expect(toCanvas).toHaveBeenCalledWith(expect.any(HTMLCanvasElement), 'https://qr.alipay.com/dynamic-order-42', expect.any(Object))

    wrapper.unmount()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    if (originalHidden) Object.defineProperty(document, 'hidden', originalHidden)
  })

  it('does not show the QR fallback after the page enters the background', async () => {
    const originalLocation = window.location
    const originalHidden = Object.getOwnPropertyDescriptor(document, 'hidden')
    let hidden = false
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { assign: vi.fn() },
    })
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => hidden,
    })

    const wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        amount: 88,
        payAmount: 88,
        qrCode: 'https://qr.alipay.com/dynamic-order-42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
        orderType: 'balance',
        outTradeNo: 'sub2_20260420abcd1234',
        mobileAlipayDeepLink: true,
      },
      global: { stubs: { Icon: true } },
    })

    await flushPromises()
    hidden = true
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(2200)
    await flushPromises()

    expect(wrapper.find('[data-test="alipay-qr-fallback"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.alipayContinueInApp')

    wrapper.unmount()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    if (originalHidden) Object.defineProperty(document, 'hidden', originalHidden)
  })
})

describe('PaymentStatusPanel desktop EasyPay QR balance lifecycle', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-11-14T00:00:00Z'))
    vi.mocked(isMobileDevice).mockReturnValue(false)
    pollOrderStatus.mockReset().mockResolvedValue(orderFactory('PENDING'))
    verifyOrder.mockReset().mockResolvedValue({ data: orderFactory('PENDING') })
    cancelOrder.mockReset().mockResolvedValue({})
    toCanvas.mockReset().mockResolvedValue(undefined)
  })

  afterEach(() => vi.useRealTimers())

  function open(overrides: Partial<InstanceType<typeof PaymentStatusPanel>['$props']> = {}) {
    return mount(PaymentStatusPanel, {
      props: {
        orderId: 42,
        qrCode: 'https://pay.example.com/qr/42',
        payUrl: 'https://pay.example.com/session/42',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'ldc',
        orderType: 'balance',
        paymentMode: 'qrcode',
        outTradeNo: 'sub2_20260420abcd1234',
        ...overrides,
      },
      global: { stubs: { Icon: true } },
    })
  }

  function deferred<T>() {
    let resolve!: (value: T) => void
    const promise = new Promise<T>((done) => { resolve = done })
    return { promise, resolve }
  }

  it.each(['EXPIRED', 'CANCELLED'])('removes QR and payment entry when verification confirms %s', async (status) => {
    verifyOrder.mockResolvedValue({ data: orderFactory(status) })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(true)
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
    await vi.advanceTimersByTimeAsync(3000)
    expect(verifyOrder).toHaveBeenCalledWith('sub2_20260420abcd1234')
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    expect(wrapper.text()).toContain(`payment.qr.${status.toLowerCase()}`)
    expect(wrapper.emitted('settled')).toEqual([[status.toLowerCase()]])
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(['EXPIRED', 'CANCELLED'])('never renders a QR while restoring provider %s', async (status) => {
    const response = deferred<{ data: ReturnType<typeof orderFactory> }>()
    verifyOrder.mockReturnValue(response.promise)
    const wrapper = open({ recovering: true })
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    await flushPromises()
    expect(toCanvas).not.toHaveBeenCalled()
    response.resolve({ data: orderFactory(status) })
    await flushPromises()
    expect(wrapper.text()).toContain(`payment.qr.${status.toLowerCase()}`)
    expect(toCanvas).not.toHaveBeenCalled()
  })

  it.each(['EXPIRED', 'CANCELLED'])('restores persisted %s without querying the provider or rendering QR', async (status) => {
    pollOrderStatus.mockResolvedValue(orderFactory(status))
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.text()).toContain(`payment.qr.${status.toLowerCase()}`)
    expect(verifyOrder).not.toHaveBeenCalled()
    expect(toCanvas).not.toHaveBeenCalled()
  })

  it('releases recovered QR and payment entry only after verified PENDING', async () => {
    const response = deferred<{ data: ReturnType<typeof orderFactory> }>()
    verifyOrder.mockReturnValue(response.promise)
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.get('[data-test="payment-verification"] button').attributes('disabled')).toBeDefined()
    response.resolve({ data: orderFactory('PENDING') })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(true)
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
    expect(wrapper.emitted('settled')).toBeUndefined()
  })

  it.each(['failure', 'unknown', 'empty', 'wrong order'])('keeps recovery blocked and retryable after %s', async (scenario) => {
    if (scenario === 'failure') verifyOrder.mockRejectedValue(new Error('temporary outage'))
    else if (scenario === 'unknown') verifyOrder.mockResolvedValue({ data: orderFactory('UNKNOWN') })
    else if (scenario === 'empty') verifyOrder.mockResolvedValue({})
    else verifyOrder.mockResolvedValue({ data: { ...orderFactory('EXPIRED'), id: 99 } })
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.text()).toContain('payment.qr.verifyUnavailable')
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    expect(toCanvas).not.toHaveBeenCalled()
    expect(wrapper.emitted('settled')).toBeUndefined()
    verifyOrder.mockResolvedValue({ data: orderFactory('PENDING') })
    await wrapper.get('[data-test="payment-verification"] button').trigger('click')
    await flushPromises()
    expect(verifyOrder).toHaveBeenCalledTimes(2)
    expect(wrapper.find('canvas').exists()).toBe(true)
  })

  it('verifies recovery even when the local order read temporarily returns null', async () => {
    pollOrderStatus.mockResolvedValue(null)
    verifyOrder.mockResolvedValue({ data: orderFactory('CANCELLED') })
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.text()).toContain('payment.qr.cancelled')
    expect(toCanvas).not.toHaveBeenCalled()
  })

  it.each(['ldc', 'alipay', 'wxpay'])('keeps %s verifying beyond six attempts and removes QR on a later terminal result', async (paymentType) => {
    verifyOrder.mockImplementation(async () => ({ data: orderFactory(verifyOrder.mock.calls.length >= 9 ? 'EXPIRED' : 'PENDING') }))
    const wrapper = open({ paymentType })
    await vi.advanceTimersByTimeAsync(108_000)
    expect(verifyOrder).toHaveBeenCalledTimes(8)
    expect(wrapper.find('canvas').exists()).toBe(true)
    await vi.advanceTimersByTimeAsync(15_000)
    expect(verifyOrder).toHaveBeenCalledTimes(9)
    expect(wrapper.text()).toContain('payment.qr.expired')
    expect(wrapper.find('canvas').exists()).toBe(false)
  })

  it.each(['UNKNOWN', 'error'])('keeps active PENDING QR after a provider %s response', async (status) => {
    if (status === 'error') verifyOrder.mockRejectedValue(new Error('temporary outage'))
    else verifyOrder.mockResolvedValue({ data: orderFactory(status) })
    const wrapper = open()
    await vi.advanceTimersByTimeAsync(18_000)
    expect(verifyOrder).toHaveBeenCalledTimes(2)
    expect(wrapper.find('canvas').exists()).toBe(true)
    expect(wrapper.emitted('settled')).toBeUndefined()
  })

  it('hides locally timed-out payment entries without settling, continues verification, then accepts PAID', async () => {
    const response = deferred<{ data: ReturnType<typeof orderFactory> }>()
    verifyOrder.mockReturnValueOnce(response.promise).mockResolvedValue({ data: orderFactory('PAID') })
    const wrapper = open({ expiresAt: new Date(Date.now() + 4000).toISOString() })
    await vi.advanceTimersByTimeAsync(4000)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    expect(wrapper.emitted('settled')).toBeUndefined()
    response.resolve({ data: { ...orderFactory('PENDING'), expires_at: new Date(Date.now() - 1000).toISOString() } })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(14_000)
    expect(wrapper.emitted('success')).toHaveLength(1)
    expect(wrapper.emitted('settled')).toEqual([['success']])
  })

  it('uses a fresh verified server deadline for an expired recovery snapshot', async () => {
    const wrapper = open({ recovering: true, expiresAt: new Date(Date.now() - 1000).toISOString() })
    expect(wrapper.find('canvas').exists()).toBe(false)
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(true)
    expect(wrapper.emitted('settled')).toBeUndefined()
  })

  it('does not overlap slow provider verification and ignores it after cancellation', async () => {
    const response = deferred<{ data: ReturnType<typeof orderFactory> }>()
    verifyOrder.mockReturnValue(response.promise)
    const wrapper = open()
    await vi.advanceTimersByTimeAsync(12_000)
    expect(pollOrderStatus).toHaveBeenCalledTimes(1)
    expect(verifyOrder).toHaveBeenCalledTimes(1)
    await wrapper.get('button.w-full').trigger('click')
    await flushPromises()
    response.resolve({ data: orderFactory('COMPLETED') })
    await flushPromises()
    expect(wrapper.emitted('settled')).toEqual([['cancelled']])
    expect(wrapper.emitted('success')).toBeUndefined()
    expect(wrapper.find('canvas').exists()).toBe(false)
  })

  it('ignores a late cancel response after payment succeeds', async () => {
    const response = deferred<object>()
    cancelOrder.mockReturnValue(response.promise)
    pollOrderStatus.mockResolvedValue(orderFactory('COMPLETED'))
    const wrapper = open()
    await wrapper.get('button.w-full').trigger('click')
    await vi.advanceTimersByTimeAsync(3000)
    response.resolve({})
    await flushPromises()
    expect(wrapper.emitted('settled')).toEqual([['success']])
    expect(wrapper.emitted('success')).toHaveLength(1)
  })

  it('ignores verification after unmount', async () => {
    const response = deferred<{ data: ReturnType<typeof orderFactory> }>()
    verifyOrder.mockReturnValue(response.promise)
    const wrapper = open({ recovering: true })
    await flushPromises()
    wrapper.unmount()
    response.resolve({ data: orderFactory('PAID') })
    await flushPromises()
    expect(wrapper.emitted('settled')).toBeUndefined()
    expect(wrapper.emitted('success')).toBeUndefined()
    expect(toCanvas).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('ignores an order read that completes after unmount', async () => {
    const response = deferred<ReturnType<typeof orderFactory>>()
    pollOrderStatus.mockReturnValue(response.promise)
    const wrapper = open({ recovering: true })
    wrapper.unmount()
    response.resolve(orderFactory('PAID'))
    await flushPromises()
    expect(verifyOrder).not.toHaveBeenCalled()
    expect(wrapper.emitted('success')).toBeUndefined()
    expect(wrapper.emitted('settled')).toBeUndefined()
  })

  it('keeps a locally overdue order retryable through repeated verification errors', async () => {
    verifyOrder.mockRejectedValue(new Error('temporary outage'))
    const wrapper = open({ expiresAt: new Date(Date.now() + 1000).toISOString() })
    await vi.advanceTimersByTimeAsync(124_000)
    expect(verifyOrder.mock.calls.length).toBeGreaterThan(6)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.verifyUnavailable')
    expect(wrapper.emitted('settled')).toBeUndefined()
    verifyOrder.mockResolvedValue({ data: orderFactory('CANCELLED') })
    await wrapper.get('[data-test="payment-verification"] button').trigger('click')
    await flushPromises()
    expect(wrapper.emitted('settled')).toEqual([['cancelled']])
  })

  it.each(['PAID', 'RECHARGING', 'COMPLETED'])('keeps the existing success event for recovered %s', async (status) => {
    verifyOrder.mockResolvedValue({ data: orderFactory(status) })
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.emitted('success')).toHaveLength(1)
    expect(wrapper.text()).toContain('payment.result.success')
    expect(toCanvas).not.toHaveBeenCalled()
  })

  it.each([
    { paymentMode: 'popup' },
    { paymentMode: 'redirect' },
    { paymentMode: 'native' },
    { paymentMode: undefined },
    { orderType: 'subscription' },
    { orderType: undefined },
    { mobile: true },
  ])('does not gate or expand custom method verification outside QR balance scope: %j', async ({ mobile, ...props }) => {
    vi.mocked(isMobileDevice).mockReturnValue(mobile === true)
    const wrapper = open({ ...props, recovering: true })
    await vi.advanceTimersByTimeAsync(123_000)
    expect(wrapper.find('canvas').exists()).toBe(true)
    expect(verifyOrder).not.toHaveBeenCalled()
  })

  it('retains the existing six verification attempts for a built-in popup method', async () => {
    const wrapper = open({ paymentType: 'alipay', paymentMode: 'popup', recovering: true })
    await vi.advanceTimersByTimeAsync(123_000)
    expect(verifyOrder).toHaveBeenCalledTimes(6)
    expect(wrapper.find('canvas').exists()).toBe(true)
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
  })
})
