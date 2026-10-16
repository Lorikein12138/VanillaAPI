import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'

import PaymentQuickAmountsEditor from '../PaymentQuickAmountsEditor.vue'
import { defaultPaymentQuickAmounts } from '@/utils/paymentQuickAmounts'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => `${key} ${JSON.stringify(params ?? {})}` }),
}))

enableAutoUnmount(afterEach)

function mountEditor(modelValue: string[] = []) {
  return mount(PaymentQuickAmountsEditor, {
    props: { modelValue },
    global: { stubs: { Icon: true } },
  })
}

describe('PaymentQuickAmountsEditor', () => {
  it('renders zero and one row, keeps an incomplete draft and removes it only on request', async () => {
    const wrapper = mountEditor()
    expect(wrapper.find('[data-testid="payment-quick-amounts-empty"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="payment-quick-amounts-count"]').text()).toContain('0')

    await wrapper.get('[data-testid="payment-quick-amounts-add"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([[['']]])
    await wrapper.setProps({ modelValue: [''] })
    expect(wrapper.findAll('[data-testid="payment-quick-amount-row"]')).toHaveLength(1)
    expect(wrapper.get('[data-testid="payment-quick-amount-error"]').text()).toContain('incomplete')
    expect(wrapper.get('[data-testid="payment-quick-amount-remove-0"]').attributes('aria-label')).toContain('remove')
    expect(wrapper.get('[data-testid="payment-quick-amounts-add"]').attributes('type')).toBe('button')
    expect(wrapper.get('[data-testid="payment-quick-amount-remove-0"]').attributes('type')).toBe('button')

    await wrapper.get('[data-testid="payment-quick-amount-remove-0"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual([[]])
  })

  it('shows duplicate and invalid row errors without removing draft text', async () => {
    const wrapper = mountEditor(['10', '10.00', '1e2'])
    expect(wrapper.findAll('[data-testid="payment-quick-amount-error"]')).toHaveLength(2)
    expect(wrapper.get('[data-testid="payment-quick-amount-input-1"]').attributes('aria-invalid')).toBe('true')
    expect((wrapper.get('[data-testid="payment-quick-amount-input-2"]').element as HTMLInputElement).value).toBe('1e2')
  })

  it('caps at twenty rows and restores defaults as a draft-only model update', async () => {
    const wrapper = mountEditor(Array.from({ length: 20 }, (_, index) => String(index + 1)))
    expect(wrapper.findAll('[data-testid="payment-quick-amount-row"]')).toHaveLength(20)
    expect(wrapper.get('[data-testid="payment-quick-amounts-count"]')).toBeDefined()
    expect((wrapper.get('[data-testid="payment-quick-amounts-add"]').element as HTMLButtonElement).disabled).toBe(true)

    await wrapper.get('[data-testid="payment-quick-amounts-reset"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual([defaultPaymentQuickAmounts().map(String)])
    expect((wrapper.get('[data-testid="payment-quick-amounts-reset"]').element as HTMLButtonElement).type).toBe('button')
  })
})
