import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import type { PaymentOrder } from '@/types/payment'
import { setSiteDisplayCurrency } from '@/utils/siteCurrency'
import AdminOrderDetail from '../AdminOrderDetail.vue'
import AdminOrderTable from '../AdminOrderTable.vue'
import AdminRefundDialog from '../AdminRefundDialog.vue'
import OrderTable from '@/components/payment/OrderTable.vue'
import AdminOrdersView from '@/views/admin/orders/AdminOrdersView.vue'

const adminPaymentAPI = vi.hoisted(() => ({ getOrders: vi.fn(), getOrder: vi.fn(), refundOrder: vi.fn(), getPlans: vi.fn() }))
vi.mock('@/api/admin/payment', () => ({ adminPaymentAPI, default: adminPaymentAPI }))
vi.mock('@/stores/app', async (importOriginal) => ({
  ...await importOriginal<typeof import('@/stores/app')>(),
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: { amount?: string }) => params?.amount ? key + ' ' + params.amount : key,
    }),
  }
})

const BaseDialogStub = {
  props: ['show'],
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
}

const DataTableStub = {
  props: ['data'],
  template: `
    <div>
      <div v-for="row in data" :key="row.id">
        <slot name="cell-pay_amount" :value="row.pay_amount" :row="row" />
      </div>
    </div>
  `,
}

function orderFactory(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 1,
    user_id: 10,
    amount: 100,
    pay_amount: 108,
    currency: 'USD',
    fee_rate: 8,
    payment_type: 'stripe',
    out_trade_no: 'sub2_202606250001',
    status: 'COMPLETED',
    order_type: 'subscription',
    created_at: '2026-06-25T10:00:00Z',
    expires_at: '2026-06-25T10:30:00Z',
    refund_amount: 25,
    ...overrides,
  }
}

describe('admin order currency display', () => {
  afterEach(() => setSiteDisplayCurrency('USD'))

  it('updates historical top-up credits and bonus while preserving legacy, USD and EUR gateway amounts', async () => {
    const wrapper = mount(OrderTable, {
      props: {
        orders: [
          orderFactory({ id: 1, order_type: 'balance', currency: undefined, amount: 10.25, pay_amount: 7.01, bonus_amount: 2.25 }),
          orderFactory({ id: 2, order_type: 'balance', currency: 'USD', amount: 12.50, pay_amount: 8.23 }),
          orderFactory({
            id: 3, order_type: 'balance', currency: 'EUR', amount: 15.20, pay_amount: 9,
            payment_info: { version: 1, actual_amount: '9.12', requires_cashier: true },
          }),
        ],
        loading: false,
      },
      global: { stubs: { DataTable: DataTableStub, OrderStatusBadge: true } },
    })

    expect(wrapper.text()).toContain('$10.25')
    setSiteDisplayCurrency('CNY')
    await nextTick()
    for (const amount of ['¥10.25', '¥2.25', '¥12.50', '¥15.20', '¥7.01', '$8.23', '€9.00', '€9.12']) {
      expect(wrapper.text()).toContain(amount)
    }
    wrapper.unmount()
  })
  it('uses the channel currency for payment amounts and omits the unknown historical plan currency', () => {
    const wrapper = mount(AdminOrderDetail, {
      props: {
        show: true,
        order: orderFactory({ currency: 'CNY', plan_id: 7 }),
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
        },
      },
    })

    const text = wrapper.text()
    expect(text).toContain('¥100.00')
    expect(text).toContain('¥8.00')
    expect(text).toContain('¥108.00')
    expect(text).toMatch(/payment\.orders\.amount\s*100\.00/)
    expect(text).toMatch(/payment\.orders\.refundCreditAmount:\s*25\.00/)
    expect(text).not.toContain('payment.orders.channelRefundAmount')
  })

  it('uses order currency for payment and the site currency for balance refunds and bonuses', () => {
    setSiteDisplayCurrency('CNY')
    const order = orderFactory({
      order_type: 'balance',
      currency: 'USD',
      status: 'PARTIALLY_REFUNDED',
      refund_amount: 20,
      bonus_amount: 5.25,
    })
    const wrapper = mount(AdminRefundDialog, {
      props: {
        show: true,
        order,
        userBalance: 200,
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
        },
      },
    })

    const text = wrapper.text()
    expect(text).toContain('$108.00')
    expect(text).toContain('payment.orders.payAmount')
    expect(text).toContain('¥100.00')
    expect(text).toMatch(/payment\.orders\.refundCreditAmount\s*¥20\.00/)
    expect(text).toContain('¥80.00')
    expect(text).toContain('¥200.00')

    const detail = mount(AdminOrderDetail, {
      props: { show: true, order },
      global: { stubs: { BaseDialog: BaseDialogStub } },
    })
    expect(detail.text()).toContain('+¥5.25')
    expect(detail.text()).toContain('$108.00')
    expect(detail.text()).toMatch(/payment\.orders\.refundCreditAmount:\s*¥20\.00/)
    detail.unmount()
    wrapper.unmount()
  })

  it('renders payment currency consistently in the shared order table', () => {
    const wrapper = mount(OrderTable, {
      props: {
        orders: [
          orderFactory({ id: 1, currency: 'USD', amount: 100, pay_amount: 108 }),
          orderFactory({ id: 2, currency: 'CNY', amount: 100, pay_amount: 108 }),
        ],
        loading: false,
        showUser: true,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          OrderStatusBadge: true,
        },
      },
    })

    const text = wrapper.text()
    expect(text).toContain('$108.00')
    expect(text).toContain('¥108.00')
    expect(text).toContain('payment.orders.amount: 100.00')
  })

  it('shows historical subscription credit without loading the current plan and preserves the refund payload', async () => {
    const order = orderFactory({ currency: 'CNY', plan_id: 7, status: 'PARTIALLY_REFUNDED', refund_amount: 20,
      payment_info: { version: 1, actual_amount: '108.1', requires_cashier: true } })
    const requestedPlan = orderFactory({ id: 2, plan_id: 7, status: 'REFUND_REQUESTED', refund_amount: 30 })
    const requestedBalance = orderFactory({ id: 3, order_type: 'balance', status: 'REFUND_REQUESTED', refund_amount: 40 })
    adminPaymentAPI.getOrders.mockResolvedValue({ data: { items: [order, requestedPlan, requestedBalance], total: 3 } })
    adminPaymentAPI.getPlans.mockResolvedValue({ data: [{ id: 7, currency: 'EUR' }] })
    adminPaymentAPI.getOrder.mockResolvedValue({ data: { order, audit_logs: [] } })
    adminPaymentAPI.refundOrder.mockResolvedValue({ data: { success: true } })

    const wrapper = mount(AdminOrdersView, { global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      BaseDialog: { props: ['show', 'title'], template: '<div v-if="show" :data-dialog="title"><slot /><slot name="footer" /></div>' },
      DataTable: {
        props: ['columns', 'data'],
        template: `<div>
          <span v-for="column in columns" :key="column.key">{{ column.label }}</span>
          <div v-for="row in data" :key="row.id">
            <slot name="cell-pay_amount" :value="row.pay_amount" :row="row" />
            <slot name="cell-actions" :row="row" />
          </div>
        </div>`,
      },
      Select: true, Pagination: true, Icon: true, OrderStatusBadge: true,
    } } })
    await flushPromises()
    setSiteDisplayCurrency('CNY')
    await nextTick()
    expect(wrapper.text()).toContain('payment.admin.colUser')
    expect(wrapper.text()).toContain('payment.orders.paymentAmount')
    expect(wrapper.get('[data-test="order-actual-amount"]').text()).toContain('payment.orders.actualPaidAmount: ¥108.10')
    expect(wrapper.text()).toContain('payment.orders.amount: 100.00')
    expect(wrapper.text()).toContain('payment.orders.refundCreditAmount: 30.00')
    expect(wrapper.text()).toContain('payment.orders.refundCreditAmount: ¥40.00')
    await wrapper.findAll('button').find(button => button.text() === 'common.view')!.trigger('click')
    await flushPromises()
    expect(adminPaymentAPI.getOrder).toHaveBeenCalledWith(order.id)
    const detail = wrapper.get('[data-dialog="payment.admin.orderDetail"]')
    expect(detail.text()).toContain('payment.orders.orderedAmount¥108.00')
    expect(detail.text()).toContain('payment.orders.actualPaidAmount¥108.10')
    expect(detail.text()).toContain('payment.orders.amount100.00')
    expect(detail.text()).toContain('payment.orders.refundCreditAmount20.00')
    expect(detail.text()).not.toContain('payment.orders.channelRefundAmount')
    expect(detail.text()).not.toContain('payment.orders.payAmount')

    await wrapper.findAll('button').find(button => button.text() === 'payment.admin.refund')!.trigger('click')
    const refund = wrapper.get('[data-dialog="payment.admin.refundOrder"]')
    expect(refund.text()).toMatch(/payment\.orders\.orderedAmount\s*¥108\.00/)
    expect(refund.text()).toMatch(/payment\.orders\.actualPaidAmount\s*¥108\.10/)
    expect(refund.text()).toMatch(/payment\.orders\.amount\s*100\.00/)
    expect(refund.text()).toMatch(/payment\.orders\.refundCreditAmount\s*20\.00/)
    expect(refund.text()).not.toContain('payment.admin.alreadyRefunded')
    expect(refund.text()).not.toContain('payment.orders.channelRefundAmount')
    expect(refund.text()).not.toContain('payment.orders.payAmount')
    expect(refund.get('input[type="number"]').attributes('max')).toBe('80')
    await refund.get('textarea').setValue('Refund reason')
    await refund.get('form').trigger('submit')
    expect(adminPaymentAPI.refundOrder).toHaveBeenCalledWith(order.id, {
      amount: 80, reason: 'Refund reason', deduct_balance: true, force: false,
    })
    await flushPromises()
    expect(adminPaymentAPI.getPlans).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('renders payment currency consistently in the admin order table', () => {
    const wrapper = mount(AdminOrderTable, {
      props: {
        orders: [
          orderFactory({ id: 1, currency: 'USD', amount: 100, pay_amount: 108 }),
          orderFactory({ id: 2, currency: 'CNY', amount: 100, pay_amount: 108 }),
        ],
        loading: false,
        page: 1,
        pageSize: 20,
        total: 2,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          Icon: true,
          Pagination: true,
          Select: true,
        },
      },
    })

    const text = wrapper.text()
    expect(text).toContain('$108.00')
    expect(text).toContain('¥108.00')
    expect(text).toContain('payment.orders.amount: 100.00')
  })
})
