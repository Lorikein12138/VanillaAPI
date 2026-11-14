import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import UserOrdersView from '../UserOrdersView.vue'
import Select from '@/components/common/Select.vue'
import Pagination from '@/components/common/Pagination.vue'

const api = vi.hoisted(() => ({ getMyOrders: vi.fn(), getRefundEligibleProviders: vi.fn() }))
vi.mock('@/api/payment', () => ({ paymentAPI: api }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))
enableAutoUnmount(afterEach)

beforeEach(() => {
  vi.clearAllMocks()
  api.getMyOrders.mockResolvedValue({ data: { items: [], total: 100 } })
  api.getRefundEligibleProviders.mockResolvedValue({ data: { provider_instance_ids: [] } })
})

async function openOrders() {
  const wrapper = mount(UserOrdersView, {
    global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      OrderTable: true, BaseDialog: true, Icon: true, Pagination: true, teleport: true
    } }
  })
  await flushPromises()
  wrapper.getComponent(Pagination).vm.$emit('update:page', 4)
  await flushPromises()
  return wrapper
}

describe('order status filtering', () => {
  it('loads the first page with the selected status', async () => {
    const wrapper = await openOrders()
    const select = wrapper.getComponent(Select)
    await select.get('button').trigger('click')
    await select.findAll('[role="option"]').find(option => option.text() === 'payment.status.pending')!.trigger('click')
    await flushPromises()
    expect(api.getMyOrders).toHaveBeenLastCalledWith({ page: 1, page_size: 20, status: 'PENDING' })
    expect(wrapper.getComponent(Pagination).props('page')).toBe(1)
    expect(api.getMyOrders).toHaveBeenCalledTimes(3)
  })

  it('keeps the current page on manual refresh', async () => {
    const wrapper = await openOrders()
    await wrapper.get('[title="common.refresh"]').trigger('click')
    await flushPromises()
    expect(api.getMyOrders).toHaveBeenLastCalledWith({ page: 4, page_size: 20, status: undefined })
  })
})

describe('user order actual, ordered and credited amounts', () => {
  it.each(['1.01', '0.99', '1.00'])('shows actual %s separately in history and refund details', async (actual) => {
    api.getMyOrders.mockResolvedValue({ data: { items: [{
      id: 42, user_id: 9, amount: 2, pay_amount: 1, currency: 'CNY', fee_rate: 0,
      payment_type: 'alipay', out_trade_no: 'order-42', status: 'COMPLETED', order_type: 'balance',
      created_at: '2026-11-14T00:00:00Z', expires_at: '2026-11-14T00:10:00Z', refund_amount: 0,
      provider_instance_id: 'provider-42', payment_info: { version: 1, actual_amount: actual, requires_cashier: true },
    }], total: 1 } })
    api.getRefundEligibleProviders.mockResolvedValue({ data: { provider_instance_ids: ['provider-42'] } })
    const wrapper = mount(UserOrdersView, { global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' }, Icon: true, Pagination: true, OrderStatusBadge: true,
      BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
    } } })
    await flushPromises()
    expect(wrapper.get('[data-test="order-actual-amount"]').text()).toContain('payment.orders.actualPaidAmount: ¥' + actual)
    expect(wrapper.get('[data-test="order-ordered-amount"]').text()).toContain('payment.orders.orderedAmount: ¥1.00')
    expect(wrapper.text()).toContain('$2.00')
    await wrapper.findAll('button').find(button => button.text() === 'payment.orders.requestRefund')!.trigger('click')
    expect(wrapper.get('[data-test="refund-actual-amount"]').text()).toContain('¥' + actual)
    expect(wrapper.get('[data-test="refund-ordered-amount"]').text()).toContain('¥1.00')
  })
})
