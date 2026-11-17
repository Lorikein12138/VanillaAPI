import { ref } from 'vue'
import type { SiteDisplayCurrency } from '@/types'

const siteDisplayCurrency = ref<SiteDisplayCurrency>('USD')

export function getSiteCurrencySymbol(): string {
  return siteDisplayCurrency.value === 'CNY' ? '¥' : '$'
}

export function setSiteDisplayCurrency(value: unknown): void {
  siteDisplayCurrency.value = value === 'CNY' ? 'CNY' : 'USD'
}
