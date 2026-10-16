export const DEFAULT_PAYMENT_QUICK_AMOUNTS = [10, 20, 50, 100, 200, 500, 1000, 2000, 5000] as const
export const MAX_PAYMENT_QUICK_AMOUNTS = 20
export const MIN_PAYMENT_QUICK_AMOUNT = 0.01
export const MAX_PAYMENT_QUICK_AMOUNT = 99_999_999.99

export type PaymentQuickAmountInputError = 'incomplete' | 'invalidAmount' | 'duplicate'

export interface ParsePaymentQuickAmountInputsResult {
  values: number[] | null
  errors: (PaymentQuickAmountInputError | null)[]
  tooMany: boolean
}

export function defaultPaymentQuickAmounts(): number[] {
  return [...DEFAULT_PAYMENT_QUICK_AMOUNTS]
}

function isValidPaymentQuickAmount(value: unknown): value is number {
  return typeof value === 'number'
    && Number.isFinite(value)
    && value >= MIN_PAYMENT_QUICK_AMOUNT
    && value <= MAX_PAYMENT_QUICK_AMOUNT
    && Number(value.toFixed(2)) === value
}

/** Return a sorted copy of valid API amounts, or a fresh default for an old or invalid response. */
export function normalizePaymentQuickAmounts(raw: unknown): number[] {
  if (!Array.isArray(raw) || raw.length > MAX_PAYMENT_QUICK_AMOUNTS) {
    return defaultPaymentQuickAmounts()
  }
  if (raw.length === 0) return []

  const cents = new Set<number>()
  const amounts: number[] = []
  for (const value of raw) {
    if (!isValidPaymentQuickAmount(value)) return defaultPaymentQuickAmounts()
    const amountCents = Math.round(value * 100)
    if (cents.has(amountCents)) return defaultPaymentQuickAmounts()
    cents.add(amountCents)
    amounts.push(value)
  }
  return amounts.sort((a, b) => a - b)
}

/** Keep every draft row in the error result; never silently drop incomplete or invalid input. */
export function parsePaymentQuickAmountInputs(
  rows: readonly string[],
): ParsePaymentQuickAmountInputsResult {
  const errors: (PaymentQuickAmountInputError | null)[] = rows.map(() => null)
  const amounts: number[] = []
  const seenCents = new Set<number>()

  rows.forEach((raw, index) => {
    const value = raw.trim()
    if (!value) {
      errors[index] = 'incomplete'
      return
    }
    if (!/^(?:\d+(?:\.\d{1,2})?|\.\d{1,2})$/.test(value)) {
      errors[index] = 'invalidAmount'
      return
    }
    const amount = Number(value)
    if (!isValidPaymentQuickAmount(amount)) {
      errors[index] = 'invalidAmount'
      return
    }
    const amountCents = Math.round(amount * 100)
    if (seenCents.has(amountCents)) {
      errors[index] = 'duplicate'
      return
    }
    seenCents.add(amountCents)
    amounts.push(amount)
  })

  const tooMany = rows.length > MAX_PAYMENT_QUICK_AMOUNTS
  const hasErrors = tooMany || errors.some((error) => error !== null)
  return {
    values: hasErrors ? null : amounts.sort((a, b) => a - b),
    errors,
    tooMany,
  }
}
