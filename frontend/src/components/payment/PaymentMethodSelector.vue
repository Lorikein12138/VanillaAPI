<template>
  <div>
    <label v-if="showLabel" class="mb-2 block text-sm text-gray-600 dark:text-dark-300">
      {{ t('payment.paymentMethod') }}
    </label>
    <div
      data-testid="payment-method-grid"
      class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3"
    >
      <button
        v-for="method in sortedMethods"
        :key="method.type"
        type="button"
        :title="methodLabel(method)"
        :disabled="!method.available"
        :aria-pressed="selected === method.type"
        :class="[
          'flex h-[60px] min-w-0 items-center gap-3 rounded-lg border px-4 text-left transition-colors',
          !method.available
            ? 'cursor-not-allowed border-gray-200 bg-gray-50 opacity-50 dark:border-dark-700 dark:bg-dark-800/50'
            : selected === method.type
              ? 'border-gray-950 bg-gray-100/70 text-gray-950 dark:border-dark-50 dark:bg-dark-700 dark:text-dark-50'
              : 'border-gray-200 bg-white text-gray-800 hover:border-gray-400 dark:border-dark-700 dark:bg-dark-800 dark:text-dark-100 dark:hover:border-dark-500',
        ]"
        @click="method.available && emit('select', method.type)"
      >
        <img :src="methodIcon(method.type)" :alt="methodLabel(method)" class="h-7 w-7 shrink-0 object-contain" />
        <span class="flex min-w-0 flex-1 flex-col items-start gap-1 leading-none">
          <span data-testid="payment-method-label" class="block w-full truncate text-[15px] font-medium">
            {{ methodLabel(method) }}
          </span>
          <span
            v-if="method.fee_rate > 0"
            class="text-[11px] text-gray-500 dark:text-dark-400"
          >
            {{ t('payment.fee') }} {{ method.fee_rate }}%
          </span>
        </span>
        <!-- 单选指示：选中为实心对勾，未选为空心圆 -->
        <span
          aria-hidden="true"
          :class="[
            'flex h-[18px] w-[18px] shrink-0 items-center justify-center rounded-full border-[1.5px]',
            selected === method.type && method.available
              ? 'border-gray-950 bg-gray-950 text-gray-50 dark:border-dark-50 dark:bg-dark-50 dark:text-dark-950'
              : 'border-gray-300 dark:border-dark-600',
          ]"
        >
          <svg v-if="selected === method.type && method.available" class="h-2.5 w-2.5" viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M2 5.2 4.2 7.4 8 2.8" /></svg>
        </span>
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { METHOD_ORDER, isBuiltInAlipayMethod, isBuiltInWxpayMethod } from './providerConfig'
import alipayIcon from '@/assets/icons/alipay.svg'
import wxpayIcon from '@/assets/icons/wxpay.svg'
import stripeIcon from '@/assets/icons/stripe.svg'
import airwallexIcon from '@/assets/icons/airwallex.svg'
import paymentIcon from '@/assets/icons/payment.svg'

export interface PaymentMethodOption {
  type: string
  display_name?: string
  fee_rate: number
  available: boolean
}

const props = withDefaults(defineProps<{
  methods: PaymentMethodOption[]
  selected: string
  /** 外部已有分区标题时隐藏组件自带标签 */
  showLabel?: boolean
}>(), {
  showLabel: true,
})

const emit = defineEmits<{
  select: [type: string]
}>()

const { t } = useI18n()

const METHOD_ICONS: Record<string, string> = {
  alipay: alipayIcon,
  wxpay: wxpayIcon,
  stripe: stripeIcon,
  airwallex: airwallexIcon,
  credit_card: paymentIcon,
}

const sortedMethods = computed(() => {
  const order: readonly string[] = METHOD_ORDER
  return [...props.methods].sort((a, b) => {
    const ai = order.indexOf(a.type)
    const bi = order.indexOf(b.type)
    return (ai === -1 ? 999 : ai) - (bi === -1 ? 999 : bi)
  })
})

function methodIcon(type: string): string {
  if (isBuiltInAlipayMethod(type)) return METHOD_ICONS.alipay
  if (isBuiltInWxpayMethod(type)) return METHOD_ICONS.wxpay
  if (type === 'airwallex') return METHOD_ICONS.airwallex
  return METHOD_ICONS[type] || paymentIcon
}

function methodLabel(method: PaymentMethodOption): string {
  return method.display_name || t(`payment.methods.${method.type}`, method.type)
}
</script>
