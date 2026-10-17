<template>
  <!-- 充值页底部「最近订单」：取最近几笔，完整列表在 /orders；请求失败时整节不渲染 -->
  <section v-if="!failed" class="flex flex-col gap-3.5" data-testid="recent-orders">
    <div class="flex items-baseline justify-between gap-3">
      <div class="flex items-baseline gap-2.5">
        <span v-if="index" class="text-xs tracking-widest tabular-nums text-primary-600 dark:text-primary-300">{{ index }}</span>
        <h2 class="font-serif text-xl font-normal text-gray-950 dark:text-dark-50">{{ t('payment.recentOrders.title') }}</h2>
      </div>
      <router-link
        to="/orders"
        class="inline-flex min-h-[44px] items-center text-[13.5px] text-gray-500 transition-colors hover:text-gray-950 dark:text-dark-400 dark:hover:text-dark-50"
        data-testid="recent-orders-view-all"
      >{{ t('payment.recentOrders.viewAll') }} →</router-link>
    </div>

    <div class="overflow-hidden rounded-[10px] border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-800">
      <p v-if="loading" class="px-6 py-10 text-center text-[13.5px] text-gray-500 dark:text-dark-400">{{ t('common.loading') }}</p>
      <p v-else-if="orders.length === 0" class="px-6 py-10 text-center text-[13.5px] text-gray-500 dark:text-dark-400" data-testid="recent-orders-empty">
        {{ t('payment.recentOrders.empty') }}
      </p>
      <template v-else>
        <div :class="['hidden border-b border-gray-200 px-6 py-3 text-xs text-gray-500 dark:border-dark-700 dark:text-dark-400 md:grid', gridClass]">
          <span>{{ t('payment.orders.createdAt') }}</span>
          <span>{{ t('payment.orders.orderType') }}</span>
          <span>{{ t('payment.orders.paymentMethod') }}</span>
          <span class="text-right">{{ t('payment.orders.payAmount') }}</span>
          <span class="text-right">{{ t('payment.orders.creditedAmount') }}</span>
          <span class="text-right">{{ t('payment.orders.status') }}</span>
        </div>
        <ul>
          <li
            v-for="row in rows"
            :key="row.id"
            class="border-t border-gray-200 first:border-t-0 dark:border-dark-700"
            data-testid="recent-order-row"
          >
            <div :class="['hidden min-h-[52px] items-center px-6 text-sm md:grid', gridClass]">
              <span class="tabular-nums text-gray-500 dark:text-dark-400">{{ row.time }}</span>
              <span class="text-gray-950 dark:text-dark-50">{{ row.type }}</span>
              <span class="text-gray-500 dark:text-dark-400">{{ row.method }}</span>
              <span class="text-right tabular-nums text-gray-950 dark:text-dark-50">{{ row.pay }}</span>
              <span :class="['text-right tabular-nums', row.credited ? 'text-gray-950 dark:text-dark-50' : 'text-gray-400 dark:text-dark-500']" data-testid="recent-order-credited">{{ row.credited || '—' }}</span>
              <span class="justify-self-end"><OrderStatusBadge :status="row.status" /></span>
            </div>
            <div class="flex items-center justify-between gap-3 px-4 py-3.5 md:hidden">
              <div class="flex min-w-0 flex-col gap-0.5">
                <span class="truncate text-sm text-gray-950 dark:text-dark-50">{{ row.type }} · {{ row.method }}</span>
                <span class="text-xs tabular-nums text-gray-500 dark:text-dark-400">{{ row.time }}</span>
              </div>
              <div class="flex shrink-0 flex-col items-end gap-1">
                <span class="text-sm tabular-nums text-gray-950 dark:text-dark-50">{{ row.pay }}</span>
                <OrderStatusBadge :status="row.status" />
              </div>
            </div>
          </li>
        </ul>
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { paymentAPI } from '@/api/payment'
import type { OrderStatus, PaymentOrder } from '@/types/payment'
import OrderStatusBadge from '@/components/payment/OrderStatusBadge.vue'
import { formatPaymentAmount } from '@/components/payment/currency'
import { formatDateTimeToMinute } from '@/utils/format'

const props = withDefaults(defineProps<{
  /** 节标题前的序号，如「03」 */
  index?: string
  limit?: number
}>(), {
  index: '',
  limit: 5,
})

const { t } = useI18n()

const orders = ref<PaymentOrder[]>([])
const loading = ref(true)
const failed = ref(false)

const gridClass = 'grid-cols-[1.3fr_1fr_1fr_1fr_1fr_0.9fr] gap-4'

// 余额已入账（含部分退款、退款处理中）的状态才显示到账金额，其余显示破折号
const CREDITED_STATUSES: OrderStatus[] = ['COMPLETED', 'PARTIALLY_REFUNDED', 'REFUND_REQUESTED', 'REFUND_FAILED']

const rows = computed(() => orders.value.map((order) => ({
  id: order.id,
  status: order.status,
  time: formatDateTimeToMinute(order.created_at),
  type: order.order_type === 'subscription' ? t('payment.admin.subscriptionOrder') : t('payment.admin.balanceOrder'),
  method: t('payment.methods.' + order.payment_type, order.payment_type),
  pay: formatPaymentAmount(order.pay_amount, order.currency),
  credited: order.order_type === 'balance' && CREDITED_STATUSES.includes(order.status) ? '+$' + order.amount.toFixed(2) : '',
})))

onMounted(async () => {
  try {
    const res = await paymentAPI.getMyOrders({ page: 1, page_size: props.limit })
    orders.value = res.data.items || []
  } catch {
    failed.value = true
  } finally {
    loading.value = false
  }
})
</script>
