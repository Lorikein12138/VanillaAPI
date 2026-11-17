import { afterEach, describe, expect, it } from 'vitest'
import { computed } from 'vue'
import { formatCurrency } from '../format'
import { formatScaled } from '../pricing'
import { setSiteDisplayCurrency } from '../siteCurrency'
import { formatTokenPricePerMillion } from '../usagePricing'

describe('formatScaled', () => {
  afterEach(() => setSiteDisplayCurrency('USD'))

  it('refreshes site price and balance formats while retaining precision and the official USD override', () => {
    const sitePrice = computed(() => formatScaled(1.25e-8, 1_000_000, 2))
    const unitPrice = computed(() => formatTokenPricePerMillion(0.0125, 1_000_000))
    const balance = computed(() => formatCurrency(0.001234))

    expect(sitePrice.value).toBe('$0.0125')
    expect(unitPrice.value).toBe('$0.0125')
    expect(balance.value).toBe('$0.001234')

    setSiteDisplayCurrency('CNY')
    expect(sitePrice.value).toBe('¥0.0125')
    expect(unitPrice.value).toBe('¥0.0125')
    expect(balance.value).toBe('¥0.001234')
    expect(formatCurrency(null)).toBe('¥0.00')
    expect(formatCurrency(3, 'EUR')).toContain('€')
    expect(formatScaled(0.000003, 1_000_000, '$', 2)).toBe('$3.00')
    expect(formatScaled(0.000003, 1_000_000, '$')).toBe('$3')
  })
  it.each([
    [1e-10, 1, '$1e-10'],
    [1.25e-10, 1, '$1.25e-10'],
    [1e-20, 1, '$1e-20'],
    [1e-16, 1_000_000, '$1e-10'],
    [1e10, 1, '$10000000000'],
  ])('preserves the magnitude of %s scaled by %s', (value, scale, expected) => {
    expect(formatScaled(value, scale)).toBe(expected)
  })

  it.each([
    [0.000003, 1_000_000, 0, '$3'],
    [0.000003, 1_000_000, 2, '$3.00'],
    [1.25e-8, 1_000_000, 2, '$0.0125'],
    [5e-8, 1_000_000, 0, '$0.05'],
    [0, 1, 2, '$0.00'],
    [null, 1_000_000, 0, '-'],
  ])('retains ordinary pricing and padding for %s', (value, scale, digits, expected) => {
    expect(formatScaled(value, scale, digits)).toBe(expected)
  })
})
