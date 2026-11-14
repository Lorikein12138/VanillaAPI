import { createServer, type Server } from 'node:http'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { apiClient } from '@/api/client'
import { paymentAPI } from '@/api/payment'
import type { OrderStatus, PaymentOrder } from '@/types/payment'
import PaymentStatusPanel from '../PaymentStatusPanel.vue'

const toCanvas = vi.hoisted(() => vi.fn())
vi.mock('qrcode', () => ({ default: { toCanvas } }))
vi.mock('@/i18n', () => ({ getLocale: () => 'en' }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))
vi.mock('@/utils/device', () => ({ isMobileDevice: () => false }))

const order: PaymentOrder = {
  id: 42,
  user_id: 9,
  amount: 10,
  pay_amount: 10,
  fee_rate: 0,
  payment_type: 'ldc',
  out_trade_no: 'sub2_terminal_sync',
  status: 'PENDING',
  order_type: 'balance',
  created_at: '2026-11-14T00:00:00Z',
  expires_at: '2099-01-01T00:00:00Z',
  refund_amount: 0,
}

// PaymentHandler.VerifyOrder -> response.ErrorFrom emits an HTTP status code in
// code and paymentVerificationUnavailable's business identifier in reason. Upstream 503,
// unknown/conflicting queries and terminal CAS failure share this same response;
// their service-to-error mapping belongs to backend tests, not duplicate UI cases.
const unavailable = {
  status: 503,
  body: {
    code: 503,
    message: 'Payment verification is temporarily unavailable; please retry.',
    reason: 'PAYMENT_VERIFICATION_UNAVAILABLE',
  },
} as const

function verified(status: OrderStatus) {
  return { status: 200, body: { code: 0, message: 'success', data: { ...order, status } } }
}

type Reply = typeof unavailable | ReturnType<typeof verified>
type RequestRecord = { method: string; path: string; body: string; authorization?: string }

describe('QR balance authenticated verification HTTP contract (R1)', () => {
  const originalBaseURL = apiClient.defaults.baseURL
  let server: Server | undefined
  let wrapper: VueWrapper | undefined
  let replies: Reply[]
  let requests: RequestRecord[]
  let unexpectedRequests: string[]
  let observedErrors: Array<{ status: number; reason: string }>
  let observer: number

  beforeEach(async () => {
    // Keep real network and timeout handling. Advance only the component's
    // intervals/deadline clock so a 15-second provider retry is deterministic.
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    toCanvas.mockReset().mockResolvedValue(undefined)
    replies = []
    requests = []
    unexpectedRequests = []
    observedErrors = []
    localStorage.setItem('auth_token', 'contract-test-token')
    observer = apiClient.interceptors.response.use(undefined, (error) => {
      observedErrors.push({ status: error.status, reason: error.reason })
      return Promise.reject(error)
    })
    server = createServer((request, response) => {
      // Exercise the browser XHR adapter against actual HTTP responses, without
      // replacing axios adapters, paymentAPI methods or the payment store.
      response.setHeader('Access-Control-Allow-Origin', request.headers.origin || '*')
      response.setHeader('Access-Control-Allow-Credentials', 'true')
      response.setHeader('Access-Control-Allow-Headers', 'Content-Type, Authorization, Accept-Language, X-User-UI-Request')
      response.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS')
      if (request.method === 'OPTIONS') {
        response.writeHead(204).end()
        return
      }
      const path = new URL(request.url || '/', 'http://fixture.test').pathname
      let body = ''
      request.setEncoding('utf8')
      request.on('data', (chunk: string) => { body += chunk })
      request.on('end', () => {
        requests.push({ method: request.method || '', path, body, authorization: request.headers.authorization })
        let reply: Reply
        if (request.method === 'GET' && path === '/api/v1/payment/orders/42') {
          // Local PENDING must never bypass an unsuccessful provider check.
          reply = verified('PENDING')
        } else if (request.method === 'POST' && path === '/api/v1/payment/orders/verify' && replies.length) {
          reply = replies.shift()!
        } else {
          unexpectedRequests.push(`${request.method} ${path}`)
          response.writeHead(500).end()
          return
        }
        response.writeHead(reply.status, { 'Content-Type': 'application/json' })
        response.end(JSON.stringify(reply.body))
      })
    })
    await new Promise<void>((resolve, reject) => {
      server!.once('error', reject)
      server!.listen(0, '127.0.0.1', resolve)
    })
    const address = server.address()
    if (!address || typeof address === 'string') throw new Error('HTTP fixture did not bind a TCP port')
    apiClient.defaults.baseURL = `http://127.0.0.1:${address.port}/api/v1`
  })

  afterEach(async () => {
    wrapper?.unmount()
    wrapper = undefined
    apiClient.interceptors.response.eject(observer)
    apiClient.defaults.baseURL = originalBaseURL
    localStorage.removeItem('auth_token')
    vi.useRealTimers()
    if (server) {
      const activeServer = server
      server = undefined
      await new Promise<void>((resolve, reject) => {
        activeServer.close((error) => error ? reject(error) : resolve())
        activeServer.closeAllConnections()
      })
    }
    expect(unexpectedRequests).toEqual([])
  })

  function open(paymentMode = 'qrcode', recovering = true) {
    wrapper = mount(PaymentStatusPanel, {
      props: {
        orderId: order.id,
        qrCode: 'weixin://test-payment',
        payUrl: 'https://example.com/pay',
        expiresAt: order.expires_at,
        outTradeNo: order.out_trade_no,
        paymentType: order.payment_type,
        paymentMode,
        orderType: order.order_type,
        recovering,
      },
      global: { plugins: [createPinia()], stubs: { Icon: true } },
    })
    return wrapper
  }

  function expectPaymentHidden({ previouslyRendered = false } = {}) {
    expect(wrapper!.find('canvas').exists()).toBe(false)
    expect(wrapper!.text()).not.toContain('payment.qr.openPayWindow')
    expect(wrapper!.emitted('settled')).toBeUndefined()
    expect(wrapper!.emitted('success')).toBeUndefined()
    if (!previouslyRendered) expect(toCanvas).not.toHaveBeenCalled()
  }

  it('rejects the shared non-2xx unavailable envelope through real paymentAPI and axios interceptors', async () => {
    replies.push(unavailable)
    await expect(paymentAPI.verifyOrder(order.out_trade_no)).rejects.toMatchObject({
      status: 503, code: 503, reason: 'PAYMENT_VERIFICATION_UNAVAILABLE',
    })
    expect(requests).toEqual([{
      method: 'POST', path: '/api/v1/payment/orders/verify',
      body: JSON.stringify({ out_trade_no: order.out_trade_no }),
      authorization: 'Bearer contract-test-token',
    }])
    expect(observedErrors).toEqual([{ status: 503, reason: 'PAYMENT_VERIFICATION_UNAVAILABLE' }])
  })

  it.each(['EXPIRED', 'CANCELLED'] as const)(
    'keeps recovery blocked on HTTP 503, retries explicit PENDING, then removes payment entries on %s', async (terminal) => {
      replies.push(unavailable, unavailable, verified('PENDING'), verified(terminal))
      const panel = open()
      expectPaymentHidden()
      await vi.waitFor(() => expect(observedErrors).toHaveLength(1))
      await vi.waitFor(() => expect(panel.text()).toContain('payment.qr.verifyUnavailable'))
      expectPaymentHidden()

      // A successful read of local PENDING between provider retries cannot
      // turn the stable unavailable error into permission to pay.
      await vi.advanceTimersByTimeAsync(3000)
      await vi.waitFor(() => expect(requests.filter((request) => request.method === 'GET')).toHaveLength(2))
      await vi.waitFor(() => expect(panel.get('[data-test="payment-verification"] button').attributes('disabled')).toBeUndefined())
      expectPaymentHidden()

      await panel.get('[data-test="payment-verification"] button').trigger('click')
      await vi.waitFor(() => expect(observedErrors).toHaveLength(2))
      await vi.waitFor(() => expect(panel.get('[data-test="payment-verification"] button').attributes('disabled')).toBeUndefined())
      expectPaymentHidden()

      await panel.get('[data-test="payment-verification"] button').trigger('click')
      await vi.waitFor(() => expect(panel.find('canvas').exists()).toBe(true))
      await flushPromises()
      expect(panel.text()).toContain('payment.qr.openPayWindow')
      expect(toCanvas).toHaveBeenCalled()
      expect(panel.emitted('settled')).toBeUndefined()

      await vi.advanceTimersByTimeAsync(15_000)
      await vi.waitFor(() => expect(panel.text()).toContain(`payment.qr.${terminal.toLowerCase()}`))
      expect(panel.find('canvas').exists()).toBe(false)
      expect(panel.text()).not.toContain('payment.qr.openPayWindow')
      expect(panel.emitted('settled')).toEqual([[terminal.toLowerCase()]])
      expect(panel.emitted('success')).toBeUndefined()
      expect(replies).toEqual([])
      expect(requests.filter((request) => request.method === 'POST')).toHaveLength(4)
    },
  )

  it.each([false, true])(
    'rebuilds the payment barrier after visible QR receives HTTP 503 (recovering=%s)', async (recovering) => {
      if (recovering) replies.push(verified('PENDING'))
      replies.push(unavailable, verified('PENDING'))
      const panel = open('qrcode', recovering)
      if (recovering) expectPaymentHidden()
      await vi.waitFor(() => expect(panel.find('canvas').exists()).toBe(true))
      await flushPromises()
      expect(toCanvas).toHaveBeenCalled()
      expect(panel.text()).toContain('payment.qr.openPayWindow')

      await vi.advanceTimersByTimeAsync(recovering ? 15_000 : 3000)
      await vi.waitFor(() => expect(observedErrors).toEqual([{ status: 503, reason: 'PAYMENT_VERIFICATION_UNAVAILABLE' }]))
      await vi.waitFor(() => expect(panel.text()).toContain('payment.qr.verifyUnavailable'))
      expectPaymentHidden({ previouslyRendered: true })
      const localReads = requests.filter((request) => request.method === 'GET').length
      const providerChecks = requests.filter((request) => request.method === 'POST').length

      await vi.advanceTimersByTimeAsync(3000)
      await vi.waitFor(() => expect(requests.filter((request) => request.method === 'GET')).toHaveLength(localReads + 1))
      await vi.waitFor(() => expect(panel.get('[data-test="payment-verification"] button').attributes('disabled')).toBeUndefined())
      expect(requests.filter((request) => request.method === 'POST')).toHaveLength(providerChecks)
      expectPaymentHidden({ previouslyRendered: true })
      expect(panel.text()).toContain('payment.qr.verifyUnavailable')

      await panel.get('[data-test="payment-verification"] button').trigger('click')
      await vi.waitFor(() => expect(panel.find('canvas').exists()).toBe(true))
      await flushPromises()
      expect(panel.text()).toContain('payment.qr.openPayWindow')
      expect(panel.emitted('settled')).toBeUndefined()
      expect(panel.emitted('success')).toBeUndefined()
      expect(requests.filter((request) => request.method === 'POST')).toHaveLength(providerChecks + 1)
      expect(replies).toEqual([])
    },
  )

  it('preserves the successful local PENDING contract and immediate payment entry outside QR mode', async () => {
    replies.push(verified('PENDING'))
    const panel = open('popup')
    expect(panel.find('canvas').exists()).toBe(true)
    expect(panel.text()).toContain('payment.qr.openPayWindow')
    expect(panel.find('[data-test="payment-verification"]').exists()).toBe(false)
    // Custom popup types retain their previous behavior: no upstream recovery
    // verification, while an explicit legacy API response remains usable.
    await expect(paymentAPI.verifyOrder(order.out_trade_no)).resolves.toMatchObject({ status: 200, data: order })
    expect(observedErrors).toEqual([])
    expect(panel.emitted('settled')).toBeUndefined()
  })
})
