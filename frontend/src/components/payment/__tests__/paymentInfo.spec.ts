import { describe, expect, it } from 'vitest'
import type { CreateOrderResult, PaymentInfo } from '@/types/payment'
import {
  decidePaymentLaunch,
  normalizePaymentInfo,
  readPaymentRecoverySnapshot,
  safeCashierUrl,
} from '../paymentFlow'

const info: PaymentInfo = { version: 1, actual_amount: '1.01', qr_content: 'weixin://pay/same-order' }
const result: CreateOrderResult = {
  order_id: 42, amount: 2, pay_amount: 1, fee_rate: 0, payment_mode: 'qrcode',
  qr_code: 'legacy-must-not-return', pay_url: 'https://cashier.example/order/42',
  expires_at: '2099-01-01T00:00:00Z', payment_info: info,
}
const context = { visibleMethod: 'wxpay', orderType: 'balance', isMobile: false } as const

describe('versioned payment info validation', () => {
  it.each(['1.01', '0.99', '1', '1.00', '999999999999.99', '0.01'])('accepts positive decimal %s without rewriting it', (amount) => {
    expect(normalizePaymentInfo({ ...info, actual_amount: amount })?.actual_amount).toBe(amount)
  })

  it.each(['0', '0.00', '-1', '+1', '01', '00.10', '.01', '1.', '1.001', '1e2', 'NaN', 'Infinity', ' 1', '1\n', '1000000000000', 1, null])(
    'rejects an unsafe actual amount %s', (amount) => {
      expect(normalizePaymentInfo({ ...info, actual_amount: amount })).toBeNull()
    },
  )

  it.each([
    null, [], 'invalid', { ...info, version: 2 }, { ...info, version: '1' },
    { ...info, qr_content: null }, { ...info, qr_content: 123 }, { ...info, qr_content: '' },
    { ...info, qr_content: 'weixin://bad\ncontent' },
    { ...info, qr_content: '<svg onload="alert(1)" />' },
    { ...info, qr_content: 'javascript:alert(1)' },
    { ...info, qr_content: 'data:image/svg+xml,test' },
    { ...info, requires_cashier: null }, { ...info, requires_cashier: 'true' },
    { ...info, expires_at: null }, { ...info, expires_at: '' },
    { ...info, expires_at: '2030-01-01' }, { ...info, expires_at: 'not-a-date' },
    { ...info, expires_at: '2030-02-30T00:00:00Z' }, { ...info, expires_at: '2030-01-01T24:00:00Z' },
    { version: 1, actual_amount: '1.01', qr_image_url: null },
    { version: 1, actual_amount: '1.01', qr_image_url: 'data:image/png;base64,x' },
  ])('fails closed for malformed objects/selected fields: %j', (value) => {
    expect(normalizePaymentInfo(value)).toBeNull()
  })

  it('allows omitted expiry and prefers content even when an unused image is invalid', () => {
    expect(normalizePaymentInfo({ ...info, qr_image_url: null })).toEqual(info)
    expect(normalizePaymentInfo({ ...info, qr_image_url: 'javascript:unsafe' })).toEqual(info)
    expect(normalizePaymentInfo({ ...info, expires_at: '2030-01-01T08:00:00+08:00' })?.expires_at).toBe('2030-01-01T08:00:00+08:00')
    expect(normalizePaymentInfo(undefined)).toBeUndefined()
  })

  it.each([
    'javascript:alert(1)', 'data:text/html,test', '//cashier.example/42', '/payment/42',
    'https:cashier.example/42', 'https:/cashier.example/42', 'https:///cashier.example/42',
    'https://user:secret@cashier.example/42', 'https:\\cashier.example/42',
    'https://cashier.example/42%0afoo', 'https://cashier.example/42%1Ffoo',
    'https://cashier.example/42%7ffoo', 'https://cashier.example/42\n',
    'https://cashier.example/42%5cfoo', 'https://cashier.example/42%C2%85foo', 'https://cashier.example/42%ff',
  ])('rejects unsafe cashier and image URLs: %s', (url) => {
    expect(safeCashierUrl(url)).toBe('')
    expect(normalizePaymentInfo({ version: 1, actual_amount: '1', qr_image_url: url })).toBeNull()
  })

  it.each(['https://cashier.example/42', 'http://cashier.example/42?key=a%20b'])('accepts safe absolute URLs: %s', (url) => {
    expect(safeCashierUrl(url)).toBe(url)
  })
})

describe('payment info launch and recovery', () => {
  it.each(['alipay', 'wxpay'])('launches %s QR with exact amount and separate ordered/credited amounts', (visibleMethod) => {
    const decision = decidePaymentLaunch(result, { ...context, visibleMethod })
    expect(decision.kind).toBe('qr_waiting')
    expect(decision.paymentState).toMatchObject({ amount: 2, payAmount: 1, qrCode: '', paymentInfo: info })
  })

  it.each([null, { ...info, version: 2 }, { ...info, actual_amount: 'bad' }, { ...info, requires_cashier: true }])(
    'never launches legacy QR for an invalid/cashier contract: %j', (paymentInfo) => {
      const decision = decidePaymentLaunch({ ...result, payment_info: paymentInfo as PaymentInfo }, context)
      expect(decision.kind).toBe('redirect_waiting')
      expect(decision.paymentState.qrCode).toBe('')
      expect(decision.paymentState.payUrl).toBe(result.pay_url)
    },
  )

  it('keeps the existing popup mode instead of switching to QR', () => {
    expect(decidePaymentLaunch({ ...result, payment_mode: 'popup' }, context).kind).toBe('redirect_waiting')
  })

  it('renders an unavailable panel when no safe QR or cashier exists instead of creating a new order', () => {
    const decision = decidePaymentLaunch({ ...result, payment_info: { ...info, requires_cashier: true }, pay_url: 'javascript:alert(1)' }, context)
    expect(decision.kind).toBe('qr_waiting')
    expect(decision.paymentState).toMatchObject({ payUrl: '', qrCode: '' })
  })

  it('round trips the contract and original absolute deadline without overwriting any amount', () => {
    const decision = decidePaymentLaunch({ ...result, payment_info: { ...info, expires_at: '2026-11-14T00:00:01Z' } }, context)
    const restored = readPaymentRecoverySnapshot(JSON.stringify(decision.recovery))
    expect(restored).toEqual(decision.recovery)
    expect(restored).toMatchObject({ amount: 2, payAmount: 1, paymentInfo: { actual_amount: '1.01', expires_at: '2026-11-14T00:00:01Z' } })
  })

  it('preserves the invalid-contract marker through storage and strips cached legacy QR/unsafe cashier', () => {
    const state = decidePaymentLaunch(result, context).recovery
    const restored = readPaymentRecoverySnapshot(JSON.stringify({ ...state, paymentInfo: null, qrCode: 'unsafe-cache', payUrl: 'javascript:unsafe' }))
    expect(restored).toMatchObject({ paymentInfo: null, qrCode: '', payUrl: '' })
  })
})
