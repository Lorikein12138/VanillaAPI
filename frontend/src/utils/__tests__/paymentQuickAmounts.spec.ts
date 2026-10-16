import { describe, expect, it } from 'vitest'

import {
  DEFAULT_PAYMENT_QUICK_AMOUNTS,
  MAX_PAYMENT_QUICK_AMOUNTS,
  MAX_PAYMENT_QUICK_AMOUNT,
  defaultPaymentQuickAmounts,
  normalizePaymentQuickAmounts,
  parsePaymentQuickAmountInputs,
} from '../paymentQuickAmounts'

describe('payment quick amount normalization', () => {
  it('uses fresh default copies for missing and invalid legacy values, while preserving an explicit empty list', () => {
    const missing = normalizePaymentQuickAmounts(undefined)
    const nullValue = normalizePaymentQuickAmounts(null)
    expect(missing).toEqual(DEFAULT_PAYMENT_QUICK_AMOUNTS)
    expect(nullValue).toEqual(DEFAULT_PAYMENT_QUICK_AMOUNTS)
    expect(missing).not.toBe(nullValue)
    missing.pop()
    expect(nullValue).toHaveLength(9)
    expect(normalizePaymentQuickAmounts([])).toEqual([])
    expect(normalizePaymentQuickAmounts([])).not.toBe(normalizePaymentQuickAmounts([]))
    expect(normalizePaymentQuickAmounts('10,20')).toEqual(DEFAULT_PAYMENT_QUICK_AMOUNTS)
  })

  it.each([
    [0],
    [-1],
    [0.001],
    [MAX_PAYMENT_QUICK_AMOUNT + 0.01],
    [1.001],
    [10, 10.00],
    Array.from({ length: MAX_PAYMENT_QUICK_AMOUNTS + 1 }, (_, index) => index + 1),
  ])('falls back to a whole default list for invalid API input %j', (raw) => {
    expect(normalizePaymentQuickAmounts(raw)).toEqual(DEFAULT_PAYMENT_QUICK_AMOUNTS)
  })

  it('sorts valid copies by amount and accepts decimal cent boundaries without mutating input', () => {
    const input = [MAX_PAYMENT_QUICK_AMOUNT, 1.15, 0.29, 0.01, 25.5]
    expect(normalizePaymentQuickAmounts(input)).toEqual([0.01, 0.29, 1.15, 25.5, MAX_PAYMENT_QUICK_AMOUNT])
    expect(input).toEqual([MAX_PAYMENT_QUICK_AMOUNT, 1.15, 0.29, 0.01, 25.5])
    expect(defaultPaymentQuickAmounts()).not.toBe(defaultPaymentQuickAmounts())
  })
})

describe('payment quick amount draft parsing', () => {
  it('sorts valid input while leaving the draft array untouched', () => {
    const rows = ['25.5', '0.29', '1.15']
    expect(parsePaymentQuickAmountInputs(rows)).toEqual({
      values: [0.29, 1.15, 25.5],
      errors: [null, null, null],
      tooMany: false,
    })
    expect(rows).toEqual(['25.5', '0.29', '1.15'])
  })

  it('retains empty, duplicate and malformed rows as indexed errors', () => {
    const result = parsePaymentQuickAmountInputs(['', '10', '10.00', '1e2', '0.001'])
    expect(result.values).toBeNull()
    expect(result.errors).toEqual(['incomplete', null, 'duplicate', 'invalidAmount', 'invalidAmount'])
    expect(result.tooMany).toBe(false)
  })

  it('rejects more than twenty rows without dropping any draft row', () => {
    const rows = Array.from({ length: MAX_PAYMENT_QUICK_AMOUNTS + 1 }, (_, index) => String(index + 1))
    const result = parsePaymentQuickAmountInputs(rows)
    expect(result.values).toBeNull()
    expect(result.tooMany).toBe(true)
    expect(result.errors).toHaveLength(21)
    expect(rows).toHaveLength(21)
  })
})
