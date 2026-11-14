import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import type { PaymentInfo, PaymentOrder } from '@/types/payment'
import PaymentStatusPanel from '../PaymentStatusPanel.vue'

const mocks = vi.hoisted(() => ({ poll: vi.fn(), verify: vi.fn(), cancel: vi.fn(), canvas: vi.fn() }))
vi.mock('@/stores/payment', () => ({ usePaymentStore: () => ({ pollOrderStatus: mocks.poll }) }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))
vi.mock('@/api/payment', () => ({ paymentAPI: { verifyOrder: mocks.verify, cancelOrder: mocks.cancel } }))
vi.mock('@/utils/device', () => ({ isMobileDevice: () => false }))
vi.mock('qrcode', () => ({ default: { toCanvas: mocks.canvas } }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ locale: 'en', t: (key: string, args?: Record<string, string>) => key + (args ? JSON.stringify(args) : '') }),
}))
enableAutoUnmount(afterEach)

const info: PaymentInfo = { version: 1, actual_amount: '1.01', qr_content: 'weixin://pay/same-order' }
function order(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 42, user_id: 9, amount: 2, pay_amount: 1, currency: 'CNY', fee_rate: 0,
    payment_type: 'wxpay', out_trade_no: 'order-42', status: 'PENDING', order_type: 'balance',
    created_at: '2026-11-14T00:00:00Z', expires_at: '2026-11-14T00:10:00Z', refund_amount: 0,
    pay_url: 'https://cashier.example/order/42', payment_info: info, ...overrides,
  }
}
type Props = InstanceType<typeof PaymentStatusPanel>['$props']
function open(overrides: Partial<Props> = {}) {
  return mount(PaymentStatusPanel, {
    props: {
      orderId: 42, amount: 2, payAmount: 1, qrCode: 'legacy-unsafe', paymentInfo: info,
      expiresAt: order().expires_at, paymentType: 'wxpay', payUrl: order().pay_url,
      outTradeNo: 'order-42', orderType: 'balance', paymentMode: 'qrcode', ...overrides,
    },
    global: { stubs: { Icon: true } },
  })
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-14T00:00:00Z'))
  mocks.poll.mockReset().mockResolvedValue(order())
  mocks.verify.mockReset().mockResolvedValue({ data: order() })
  mocks.canvas.mockReset().mockResolvedValue(undefined)
  mocks.cancel.mockReset().mockResolvedValue({})
})
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

describe('versioned payment QR display and fallback', () => {
  it.each(['alipay', 'wxpay'].flatMap(method => ['1.01', '0.99', '1.00'].map(actual => [method, actual])))('%s keeps actual %s separate from order 1.00 and credited 2.00', async (method, actual) => {
    const wrapper = open({ paymentType: method, paymentInfo: { ...info, actual_amount: actual } })
    await flushPromises()
    expect(wrapper.get('[data-test="actual-amount"]').text()).toBe('¥' + actual)
    expect(wrapper.get('[data-test="payment-amounts"]').text()).toContain('"ordered":"¥1.00"')
    expect(wrapper.get('[data-test="payment-amounts"]').text()).toContain('"credited":"$2.00"')
    expect(mocks.canvas).toHaveBeenCalledWith(expect.any(HTMLCanvasElement), info.qr_content, expect.any(Object))
  })

  it('selects raw content before an unused invalid image and never treats content as an image URL', async () => {
    const content = 'https://pay.example/opaque-payment-content'
    const wrapper = open({ paymentInfo: { ...info, qr_content: content, qr_image_url: 'javascript:unused' } })
    await flushPromises()
    expect(wrapper.find('[data-test="payment-qr-image"]').exists()).toBe(false)
    expect(mocks.canvas).toHaveBeenCalledWith(expect.any(HTMLCanvasElement), content, expect.any(Object))
  })

  it('displays an image directly and falls back to the same cashier when loading fails', async () => {
    const paymentInfo: PaymentInfo = { version: 1, actual_amount: '1.01', qr_image_url: 'https://cdn.example/order-42.png' }
    mocks.poll.mockResolvedValue(order({ payment_info: paymentInfo }))
    mocks.verify.mockResolvedValue({ data: order({ payment_info: paymentInfo }) })
    const openSpy = vi.spyOn(window, 'open').mockReturnValue({ closed: false } as Window)
    const wrapper = open({ paymentInfo })
    await flushPromises()
    const image = wrapper.get('[data-test="payment-qr-image"]')
    expect(image.attributes('src')).toBe(paymentInfo.qr_image_url)
    expect(mocks.canvas).not.toHaveBeenCalled()
    await image.trigger('error')
    expect(wrapper.find('[data-test="payment-qr-image"]').exists()).toBe(false)
    await wrapper.get('button.btn.btn-secondary.text-sm').trigger('click')
    expect(openSpy).toHaveBeenCalledWith(order().pay_url, 'paymentPopup', expect.any(String))
    await vi.advanceTimersByTimeAsync(18_000)
    expect(wrapper.find('[data-test="payment-qr-image"]').exists()).toBe(false)
    expect(mocks.canvas).not.toHaveBeenCalled()
    expect(wrapper.emitted('settled')).toBeUndefined()
  })

  it('catches QR generation failure and offers the same order cashier without retrying the broken code', async () => {
    mocks.canvas.mockRejectedValue(new Error('QR too large'))
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
    const count = mocks.canvas.mock.calls.length
    await vi.advanceTimersByTimeAsync(18_000)
    expect(mocks.canvas).toHaveBeenCalledTimes(count)
    expect(wrapper.emitted('settled')).toBeUndefined()
  })

  it.each([null, { ...info, version: 2 }, { ...info, actual_amount: '0' }, { ...info, expires_at: 'bad' }, { ...info, requires_cashier: true }])(
    'suppresses legacy QR for invalid or cashier contracts: %j', async (paymentInfo) => {
      const wrapper = open({ paymentInfo: paymentInfo as PaymentInfo })
      await flushPromises()
      expect(wrapper.find('canvas').exists()).toBe(false)
      expect(mocks.canvas).not.toHaveBeenCalled()
      expect(wrapper.text()).toContain('payment.qr.openPayWindow')
    },
  )

  it('shows unavailable without spinner or payment entrance when no safe same-order cashier exists', async () => {
    const wrapper = open({ paymentInfo: { ...info, requires_cashier: true }, payUrl: 'javascript:unsafe' })
    await flushPromises()
    expect(wrapper.get('[data-test="qr-unavailable"]').text()).toContain('payment.qr.unavailable')
    expect(wrapper.find('.animate-spin').exists()).toBe(false)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
  })

  it.each(['', 'https:cashier.example/42'])('does not show even an otherwise valid QR without a safe cashier URL: %s', async (payUrl) => {
    const wrapper = open({ payUrl })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(mocks.canvas).not.toHaveBeenCalled()
    expect(wrapper.get('[data-test="qr-unavailable"]').text()).toContain('payment.qr.unavailable')
    expect(wrapper.find('.animate-spin').exists()).toBe(false)
  })
})

describe('versioned QR deadlines and authoritative recovery', () => {
  it('expires only the QR, keeps the order polling, and never revives it on later PENDING with a new deadline', async () => {
    const paymentInfo = { ...info, expires_at: '2026-11-14T00:00:02Z' }
    const wrapper = open({ paymentInfo })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.codeExpired')
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
    await vi.advanceTimersByTimeAsync(16_000)
    expect(mocks.verify).toHaveBeenCalled()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.emitted('settled')).toBeUndefined()
    expect(wrapper.emitted('success')).toBeUndefined()
  })

  it('uses the absolute order deadline when QR expiry is omitted, without settling on local timeout', async () => {
    const wrapper = open({ expiresAt: '2026-11-14T00:00:02Z' })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.emitted('settled')).toBeUndefined()
    await vi.advanceTimersByTimeAsync(1000)
    expect(mocks.poll).toHaveBeenCalled()
    expect(wrapper.find('canvas').exists()).toBe(false)
  })

  it.each(['PAID', 'RECHARGING', 'COMPLETED', 'CANCELLED', 'EXPIRED', 'FAILED', 'REFUNDED'] as const)('removes QR and cashier on known terminal %s', async (status) => {
    mocks.poll.mockResolvedValue(order({ status }))
    const wrapper = open()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    expect(wrapper.emitted('settled')).toHaveLength(1)
    expect(vi.getTimerCount()).toBe(0)
    if (['PAID', 'RECHARGING', 'COMPLETED'].includes(status)) {
      expect(wrapper.get('[data-test="actual-paid-amount"]').text()).toContain('¥1.01')
      expect(wrapper.text()).toContain('payment.orders.orderedAmount')
    }
  })

  it('keeps recovery blocked until verified PENDING, then uses verified amount/code/URL rather than cache or stale auth read', async () => {
    let resolve!: (value: { data: PaymentOrder }) => void
    mocks.verify.mockReturnValue(new Promise(done => { resolve = done }))
    const wrapper = open({ recovering: true, amount: 999, payAmount: 888,
      paymentInfo: { ...info, actual_amount: '777', qr_content: 'cached-code', expires_at: '2025-01-01T00:00:00Z' }, payUrl: 'https://cached.example/order/42' })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('777')
    const verified = order({ amount: 3, pay_amount: 1.5, payment_info: { ...info, actual_amount: '1.51', qr_content: 'verified-code' }, pay_url: 'https://cashier.example/verified-42' })
    resolve({ data: verified })
    await flushPromises()
    expect(wrapper.get('[data-test="actual-amount"]').text()).toBe('¥1.51')
    expect(wrapper.text()).toContain('"ordered":"¥1.50"')
    expect(wrapper.text()).toContain('"credited":"$3.00"')
    expect(mocks.canvas).toHaveBeenCalledWith(expect.any(HTMLCanvasElement), 'verified-code', expect.any(Object))
    expect(wrapper.emitted('orderUpdated')?.at(-1)).toEqual([verified])
  })

  it('does not restore cached/legacy code when server still requires cashier', async () => {
    const verified = order({ qr_code: 'server-legacy', payment_info: { ...info, requires_cashier: true } })
    mocks.poll.mockResolvedValue(verified)
    mocks.verify.mockResolvedValue({ data: verified })
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(mocks.canvas).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
  })

  it('does not trust a recovery snapshot when the authenticated order omits its extension and payment entries', async () => {
    const verified = order({ payment_info: undefined, pay_url: undefined, qr_code: undefined })
    mocks.poll.mockResolvedValue(verified)
    mocks.verify.mockResolvedValue({ data: verified })
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.unavailable')
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
  })

  it.each(['error', 'wrong-order', 'empty', 'unknown'])('keeps verification barrier on %s and never displays cached QR', async (scenario) => {
    if (scenario === 'error') mocks.verify.mockRejectedValue(new Error('unavailable'))
    if (scenario === 'wrong-order') mocks.verify.mockResolvedValue({ data: order({ id: 99 }) })
    if (scenario === 'empty') mocks.verify.mockResolvedValue({})
    if (scenario === 'unknown') mocks.verify.mockResolvedValue({ data: order({ status: 'UNKNOWN' as PaymentOrder['status'] }) })
    const wrapper = open({ recovering: true })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    expect(wrapper.emitted('settled')).toBeUndefined()
  })
})
