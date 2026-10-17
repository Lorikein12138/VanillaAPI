<template>
  <div>
    <!-- 快捷金额与「自定义金额」同处一个网格；自定义格内嵌输入框 -->
    <div v-if="filteredAmounts.length > 0">
      <p class="sr-only" data-testid="quick-amounts-label">{{ t('payment.quickAmounts') }}</p>
      <div class="grid grid-cols-2 gap-3 sm:grid-cols-5">
        <button
          v-for="amt in filteredAmounts"
          :key="amt"
          type="button"
          :aria-pressed="modelValue === amt"
          :class="[
            'flex min-h-[76px] min-w-0 flex-col justify-between gap-2 rounded-lg border px-3.5 pb-3 pt-3.5 text-left transition-colors',
            modelValue === amt ? tileSelectedClass : tileIdleClass,
          ]"
          :data-testid="`quick-amount-${amt}`"
          @click="selectAmount(amt)"
        >
          <span class="flex w-full items-start justify-between gap-1.5">
            <span class="font-serif text-2xl font-normal leading-none tabular-nums text-gray-950 dark:text-dark-50">
              <span class="mr-0.5 text-sm text-gray-500 dark:text-dark-400">{{ symbol }}</span>{{ amt }}
            </span>
            <!-- 优惠价签：仅命中档位的金额显示 -->
            <span
              v-if="quoteFor(amt).percent > 0"
              class="shrink-0 whitespace-nowrap rounded-full bg-primary-600/10 px-1.5 py-px text-[11px] font-medium text-primary-600 dark:bg-primary-300/15 dark:text-primary-300"
              data-testid="quick-amount-bonus-badge"
            >{{ badgeText(amt) }}</span>
          </span>
          <!-- 配置了优惠阶梯时，所有按钮都显示第二行，保持高度一致：赠金显示到账 USD，折扣显示折后实付 -->
          <span
            v-if="showSecondLine"
            :class="[
              'block truncate text-xs tabular-nums',
              quoteFor(amt).percent > 0 ? 'text-primary-600 dark:text-primary-300' : 'text-gray-500 dark:text-dark-400',
            ]"
            data-testid="quick-amount-credited"
          >{{ secondLine(amt) }}</span>
        </button>
        <label
          :class="[
            'flex min-h-[76px] min-w-0 cursor-text flex-col justify-between gap-2 rounded-lg border px-3.5 pb-3 pt-3.5 transition-colors',
            isCustom ? tileSelectedClass : tileIdleClass,
          ]"
          data-testid="custom-amount"
        >
          <span class="text-xs text-gray-500 dark:text-dark-400">{{ t('payment.customAmount') }}</span>
          <span class="flex items-baseline gap-0.5 border-b border-gray-200 focus-within:border-gray-950 dark:border-dark-600 dark:focus-within:border-dark-200">
            <span class="text-sm text-gray-500 dark:text-dark-400">{{ symbol }}</span>
            <input
              type="text"
              inputmode="decimal"
              :value="customText"
              :placeholder="placeholderText"
              class="w-full min-w-0 border-0 bg-transparent p-0 pb-1 font-serif text-lg tabular-nums text-gray-950 placeholder:font-sans placeholder:text-sm placeholder:text-gray-400 focus:outline-none focus:ring-0 dark:text-dark-50 dark:placeholder:text-dark-500"
              @input="handleInput"
            />
          </span>
        </label>
      </div>
    </div>

    <!-- 未配置快捷金额：只保留自定义金额输入 -->
    <div v-else>
      <label for="recharge-custom-amount" class="mb-2 block text-sm text-gray-600 dark:text-dark-300">
        {{ t('payment.customAmount') }}
      </label>
      <div class="relative">
        <span class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-500 dark:text-dark-400">
          {{ symbol }}
        </span>
        <input
          id="recharge-custom-amount"
          type="text"
          inputmode="decimal"
          :value="customText"
          :placeholder="placeholderText"
          class="input w-full py-3 pl-8 pr-4 font-serif text-lg tabular-nums"
          @input="handleInput"
        />
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { RechargeBonusTier } from '@/types/payment'
import { formatRechargeBonusNumber, quoteRechargeBonus, type RechargeBonusMode } from '@/utils/rechargeBonus'
import { normalizePaymentQuickAmounts } from '@/utils/paymentQuickAmounts'
import { currencySymbol, formatPaymentAmount } from './currency'

const props = withDefaults(defineProps<{
  amounts?: number[]
  modelValue: number | null
  min?: number
  max?: number
  /** 充值优惠阶梯（按 min_amount 升序）；为空时不显示价签与第二行 */
  bonusTiers?: RechargeBonusTier[]
  /** 阶梯模式：bonus 赠金 / discount 折扣 */
  bonusMode?: RechargeBonusMode
  /** 充值倍率（1 支付币种 = multiplier USD），用于计算到账金额 */
  multiplier?: number
  /** 支付币种（折扣模式第二行实付金额的币种与精度） */
  currency?: string
}>(), {
  amounts: () => normalizePaymentQuickAmounts(undefined),
  min: 0,
  max: 0,
  bonusTiers: () => [],
  bonusMode: 'bonus',
  multiplier: 1,
  currency: undefined,
})

const emit = defineEmits<{
  'update:modelValue': [value: number | null]
}>()

const { t } = useI18n()

const customText = ref('')

// 0 = no limit
const filteredAmounts = computed(() =>
  props.amounts.filter((a) => (props.min <= 0 || a >= props.min) && (props.max <= 0 || a <= props.max))
)

const showSecondLine = computed(() => props.bonusTiers.length > 0)

// 金额按支付币种输入，前缀跟随币种（默认人民币）
const symbol = computed(() => currencySymbol(props.currency))

// 当前金额不是任何快捷金额时，高亮「自定义金额」格
const isCustom = computed(() => props.modelValue !== null && !filteredAmounts.value.includes(props.modelValue))

const tileSelectedClass = 'border-gray-950 bg-gray-100/70 dark:border-dark-50 dark:bg-dark-700'
const tileIdleClass = 'border-gray-200 bg-white hover:border-gray-400 dark:border-dark-700 dark:bg-dark-800 dark:hover:border-dark-500'

function currencyDigits(): number {
  if (!props.currency) return 2
  try {
    return new Intl.NumberFormat(undefined, { style: 'currency', currency: props.currency }).resolvedOptions().maximumFractionDigits ?? 2
  } catch {
    return 2
  }
}

function quoteFor(amt: number) {
  return quoteRechargeBonus(props.bonusTiers, amt, {
    multiplier: props.multiplier,
    mode: props.bonusMode,
    currencyDigits: currencyDigits(),
  })
}

// 价签文案：赠金「+20%」，折扣「20% OFF」
function badgeText(amt: number): string {
  const percent = formatRechargeBonusNumber(quoteFor(amt).percent)
  return props.bonusMode === 'discount' ? `${percent}% OFF` : `+${percent}%`
}

function secondLine(amt: number): string {
  const quote = quoteFor(amt)
  if (props.bonusMode === 'discount') {
    return t('payment.rechargeBonus.payShort', { amount: formatPaymentAmount(quote.payBase, props.currency) })
  }
  return t('payment.rechargeBonus.creditedShort', { amount: '$' + quote.credited.toFixed(2) })
}

const placeholderText = computed(() => {
  if (props.min > 0 && props.max > 0) return `${props.min} - ${props.max}`
  if (props.min > 0) return `≥ ${props.min}`
  if (props.max > 0) return `≤ ${props.max}`
  return t('payment.enterAmount')
})

const AMOUNT_PATTERN = /^\d*(\.\d{0,2})?$/

function selectAmount(amt: number) {
  customText.value = String(amt)
  emit('update:modelValue', amt)
}

function handleInput(e: Event) {
  const input = e.target as HTMLInputElement
  const val = input.value
  if (!AMOUNT_PATTERN.test(val)) {
    input.value = customText.value
    return
  }
  customText.value = val
  if (val === '') {
    emit('update:modelValue', null)
    return
  }
  const num = parseFloat(val)
  if (!isNaN(num) && num > 0) {
    emit('update:modelValue', num)
  } else {
    emit('update:modelValue', null)
  }
}

watch(() => props.modelValue, (v) => {
  if (v !== null && String(v) !== customText.value) {
    customText.value = String(v)
  }
}, { immediate: true })
</script>
