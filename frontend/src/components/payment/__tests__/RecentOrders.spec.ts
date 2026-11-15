import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import RecentOrders from '@/components/payment/RecentOrders.vue'
import type { PaymentOrder } from '@/types/payment'

const getMyOrders = vi.hoisted(() => vi.fn())

vi.mock('@/api/payment', () => ({
  paymentAPI: { getMyOrders },
}))

vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({
    t: (key: string, fallback?: string) => (typeof fallback === 'string' ? fallback : key),
  }),
}))

function order(overrides: Partial<PaymentOrder>): PaymentOrder {
  return {
    id: 1,
    user_id: 1,
    amount: 100,
    pay_amount: 100,
    currency: 'CNY',
    fee_rate: 0,
    payment_type: 'alipay',
    out_trade_no: 'T1',
    status: 'COMPLETED',
    order_type: 'balance',
    created_at: '2026-10-17T13:42:00Z',
    expires_at: '2026-10-17T14:12:00Z',
    refund_amount: 0,
    ...overrides,
  }
}

function mountOrders() {
  return mount(RecentOrders, {
    props: { index: '03' },
    global: { stubs: { RouterLink: { props: ['to'], template: '<a :href="to"><slot /></a>' } } },
  })
}

describe('RecentOrders', () => {
  beforeEach(() => {
    getMyOrders.mockReset()
  })

  it('requests the latest five orders and links to the full list', async () => {
    getMyOrders.mockResolvedValue({ data: { items: [], total: 0 } })
    const wrapper = mountOrders()
    await flushPromises()
    expect(getMyOrders).toHaveBeenCalledWith({ page: 1, page_size: 5 })
    expect(wrapper.get('[data-testid="recent-orders-view-all"]').attributes('href')).toBe('/orders')
  })

  it('shows the empty state when the user has no orders', async () => {
    getMyOrders.mockResolvedValue({ data: { items: [], total: 0 } })
    const wrapper = mountOrders()
    await flushPromises()
    expect(wrapper.find('[data-testid="recent-orders-empty"]').exists()).toBe(true)
    expect(wrapper.findAll('[data-testid="recent-order-row"]')).toHaveLength(0)
  })

  it('shows credited USD only for balance orders that were credited', async () => {
    getMyOrders.mockResolvedValue({
      data: {
        items: [
          order({ id: 1, amount: 110, pay_amount: 100 }),
          order({ id: 2, status: 'CANCELLED' }),
          order({ id: 3, order_type: 'subscription', amount: 199, pay_amount: 199 }),
        ],
        total: 3,
      },
    })
    const wrapper = mountOrders()
    await flushPromises()
    const rows = wrapper.findAll('[data-testid="recent-order-row"]')
    expect(rows).toHaveLength(3)
    expect(rows[0].get('[data-testid="recent-order-credited"]').text()).toBe('+$110.00')
    expect(rows[1].get('[data-testid="recent-order-credited"]').text()).toBe('—')
    expect(rows[2].get('[data-testid="recent-order-credited"]').text()).toBe('—')
    expect(rows[0].text()).toContain('alipay')
  })

  it('uses a fixed heading and expands only floating orders in desktop and mobile history', async () => {
    const paymentInfo = { version: 1, requires_cashier: true } as const
    getMyOrders.mockResolvedValue({ data: { items: [
      order({ id: 1, amount: 19.5, pay_amount: 19.5 }),
      order({ id: 2, amount: 19.5, pay_amount: 19.5, payment_info: { ...paymentInfo, actual_amount: '19.50' } }),
      order({ id: 3, amount: 19.5, pay_amount: 19.5, payment_info: { ...paymentInfo, actual_amount: 'invalid' } }),
      order({ id: 4, amount: 2, pay_amount: 1, payment_info: { ...paymentInfo, actual_amount: '1.1' } }),
      order({ id: 5, amount: 2, pay_amount: 1, status: 'PENDING', payment_info: { ...paymentInfo, actual_amount: '0.99' } }),
    ], total: 5 } })
    const wrapper = mountOrders()
    await flushPromises()
    const rows = wrapper.findAll('[data-testid="recent-order-row"]')
    expect(wrapper.text()).toContain('payment.orders.paymentAmount')
    for (const row of rows.slice(0, 3)) {
      expect(row.text()).not.toContain('payment.orders.orderedAmount')
      expect(row.text().match(/¥19\.50/g)).toHaveLength(2)
      expect(row.get('div.md\\:hidden').text()).toContain('payment.orders.creditedAmount: +$19.50')
    }
    for (const [index, label, amount] of [
      [3, 'actualPaidAmount', '1.10'],
      [4, 'actualPayAmount', '0.99'],
    ] as const) {
      for (const layout of ['div.md\\:grid', 'div.md\\:hidden']) {
        const text = rows[index].get(layout).text()
        expect(text).toContain('payment.orders.' + label + ': ¥' + amount)
        expect(text).toContain('payment.orders.orderedAmount: ¥1.00')
      }
    }
    expect(rows[3].get('[data-testid="recent-order-credited"]').text()).toBe('+$2.00')
    expect(rows[4].get('[data-testid="recent-order-credited"]').text()).toBe('—')
    expect(rows[4].get('div.md\\:hidden').text()).not.toContain('payment.orders.creditedAmount')
  })

  it('hides the whole section when the request fails', async () => {
    getMyOrders.mockRejectedValue(new Error('network'))
    const wrapper = mountOrders()
    await flushPromises()
    expect(wrapper.find('[data-testid="recent-orders"]').exists()).toBe(false)
  })
})
