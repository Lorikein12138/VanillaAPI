import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import type { PaymentOrder } from '@/types/payment'
import PaymentQRCodeView from '../PaymentQRCodeView.vue'

const mocks = vi.hoisted(() => ({ get: vi.fn(), poll: vi.fn(), verify: vi.fn(), canvas: vi.fn(), push: vi.fn() }))
const route = vi.hoisted(() => ({ query: {} as Record<string, string> }))
vi.mock('vue-router', () => ({ useRoute: () => route, useRouter: () => ({ push: mocks.push }) }))
vi.mock('@/stores/payment', () => ({ usePaymentStore: () => ({ pollOrderStatus: mocks.poll }) }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))
vi.mock('@/api/payment', () => ({ paymentAPI: { getOrder: mocks.get, verifyOrder: mocks.verify } }))
vi.mock('@/utils/device', () => ({ isMobileDevice: () => false }))
vi.mock('qrcode', () => ({ default: { toCanvas: mocks.canvas } }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key }),
}))
enableAutoUnmount(afterEach)

function order(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 42, user_id: 9, amount: 2, pay_amount: 1, fee_rate: 0, payment_type: 'custom',
    out_trade_no: 'order-42', status: 'PENDING', order_type: 'balance', refund_amount: 0,
    created_at: '2026-11-14T00:00:00Z', expires_at: '2026-11-14T00:10:00Z',
    pay_url: 'https://cashier.example/42', qr_code: 'server-legacy',
    payment_info: { version: 1, actual_amount: '1.01', qr_content: 'server-raw-code' }, ...overrides,
  }
}
function open() {
  return mount(PaymentQRCodeView, { global: { stubs: { AppLayout: { template: '<div><slot /></div>' }, Icon: true } } })
}
beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-14T00:00:00Z'))
  localStorage.clear()
  route.query = { order_id: '42', qr: 'query-must-not-override', pay_url: 'https://query.example/42', expires_at: '2099-01-01T00:00:00Z' }
  mocks.get.mockReset().mockResolvedValue({ data: order() })
  mocks.poll.mockReset().mockResolvedValue(order())
  mocks.verify.mockReset().mockResolvedValue({ data: order() })
  mocks.canvas.mockReset().mockResolvedValue(undefined)
  mocks.push.mockReset()
})
afterEach(() => vi.useRealTimers())

describe('reachable /payment/qrcode uses server presentation', () => {
  it('never renders query QR while authenticated details are pending; then prefers verified server extension', async () => {
    let resolve!: (value: { data: PaymentOrder }) => void
    mocks.get.mockReturnValue(new Promise(done => { resolve = done }))
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(mocks.canvas).not.toHaveBeenCalled()
    resolve({ data: order() })
    await flushPromises()
    expect(mocks.get).toHaveBeenCalledWith(42)
    expect(mocks.verify).toHaveBeenCalledWith('order-42')
    expect(wrapper.get('[data-test="actual-amount"]').text()).toBe('¥1.01')
    expect(mocks.canvas).toHaveBeenCalledWith(expect.any(HTMLCanvasElement), 'server-raw-code', expect.any(Object))
    expect(mocks.canvas.mock.calls.every(call => call[1] !== 'query-must-not-override')).toBe(true)
  })

  it('keeps server requires_cashier authoritative over query and legacy server code', async () => {
    const pending = order({ payment_info: { version: 1, actual_amount: '1.01', requires_cashier: true } })
    mocks.get.mockResolvedValue({ data: pending })
    mocks.poll.mockResolvedValue(pending)
    mocks.verify.mockResolvedValue({ data: pending })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
    expect(mocks.canvas).not.toHaveBeenCalled()
  })

  it('shows unavailable on authentication failure and does not fall back to query payment entries', async () => {
    mocks.get.mockRejectedValue(new Error('Unauthorized'))
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.find('a').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.unavailable')
  })

  it('uses the server order deadline when extension expiry is absent and never restores QR after PENDING', async () => {
    const pending = order({ expires_at: '2026-11-14T00:00:02Z' })
    mocks.get.mockResolvedValue({ data: pending })
    mocks.poll.mockResolvedValue(pending)
    mocks.verify.mockResolvedValue({ data: pending })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.codeExpired')
    expect(wrapper.text()).not.toContain('payment.qr.expiredDesc')
    await vi.advanceTimersByTimeAsync(16_000)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(mocks.push).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('payment.qr.openPayWindow')
  })

  it.each(['PAID', 'CANCELLED', 'EXPIRED', 'FAILED'] as const)('never renders query or cached extension when server is already %s', async (status) => {
    mocks.get.mockResolvedValue({ data: order({ status }) })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    expect(mocks.canvas).not.toHaveBeenCalled()
  })

  it('removes visible legacy QR and cashier on a later terminal order read', async () => {
    const pending = order({ payment_info: undefined })
    mocks.get.mockResolvedValue({ data: pending })
    mocks.poll.mockResolvedValue({ ...pending, status: 'CANCELLED' })
    const wrapper = open()
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(true)
    await vi.advanceTimersByTimeAsync(3000)
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.find('a').exists()).toBe(false)
    expect(wrapper.text()).toContain('payment.qr.expired')
  })

  it('retains legacy query compatibility only after a server PENDING order without an extension', async () => {
    mocks.get.mockResolvedValue({ data: order({ payment_info: undefined, qr_code: undefined, pay_url: undefined, expires_at: '2026-11-14T00:01:00Z' }) })
    const wrapper = open()
    await flushPromises()
    expect(mocks.canvas).toHaveBeenCalledWith(expect.any(HTMLCanvasElement), 'query-must-not-override', expect.any(Object))
    expect(wrapper.text()).toContain('01:00')
  })

  it('keeps terminal state when an earlier authenticated PENDING read arrives late', async () => {
    let resolve!: (value: { data: PaymentOrder }) => void
    mocks.get.mockReturnValue(new Promise(done => { resolve = done }))
    mocks.poll.mockResolvedValue(order({ status: 'CANCELLED' }))
    const wrapper = open()
    await vi.advanceTimersByTimeAsync(3000)
    resolve({ data: order() })
    await flushPromises()
    expect(wrapper.find('canvas').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('payment.qr.openPayWindow')
    expect(mocks.canvas).not.toHaveBeenCalled()
  })
})
