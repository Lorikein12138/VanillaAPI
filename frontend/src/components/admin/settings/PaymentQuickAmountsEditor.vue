<template>
  <div class="card" data-testid="payment-quick-amounts-editor">
    <div class="border-b border-gray-100 px-4 py-3 dark:border-dark-700">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="min-w-0">
          <h2 class="text-lg font-serif font-normal text-gray-950 dark:text-dark-50">
            {{ t('admin.settings.payment.quickAmounts.label') }}
          </h2>
          <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.settings.payment.quickAmounts.hint') }}
          </p>
        </div>
        <div class="flex shrink-0 items-center gap-2">
          <span class="text-xs text-gray-500 dark:text-gray-400" data-testid="payment-quick-amounts-count">
            {{ t('admin.settings.payment.quickAmounts.count', { count: rows.length, max: MAX_PAYMENT_QUICK_AMOUNTS }) }}
          </span>
          <button
            type="button"
            class="btn btn-secondary btn-sm"
            data-testid="payment-quick-amounts-reset"
            @click="resetDraft"
          >{{ t('admin.settings.payment.quickAmounts.reset') }}</button>
          <button
            type="button"
            class="btn btn-primary btn-sm"
            :disabled="rows.length >= MAX_PAYMENT_QUICK_AMOUNTS"
            data-testid="payment-quick-amounts-add"
            @click="addRow"
          >
            <Icon name="plus" size="sm" class="mr-1" />
            {{ t('admin.settings.payment.quickAmounts.add') }}
          </button>
        </div>
      </div>
    </div>

    <div class="space-y-3 p-4">
      <p
        v-if="rows.length === 0"
        class="rounded-xl border border-dashed border-gray-300 px-4 py-3 text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
        data-testid="payment-quick-amounts-empty"
      >{{ t('admin.settings.payment.quickAmounts.empty') }}</p>
      <div v-else class="overflow-hidden rounded-xl border border-gray-200 dark:border-dark-600">
        <div class="grid grid-cols-[minmax(0,1fr)_2rem] items-center gap-3 bg-gray-50 px-3 py-2 text-xs font-medium text-gray-500 dark:bg-dark-800 dark:text-gray-400">
          <span>{{ t('admin.settings.payment.quickAmounts.amountLabel') }}</span>
          <span></span>
        </div>
        <div class="divide-y divide-gray-100 dark:divide-dark-700">
          <div
            v-for="(row, index) in rows"
            :key="index"
            class="grid grid-cols-[minmax(0,1fr)_2rem] items-center gap-3 px-3 py-2"
            data-testid="payment-quick-amount-row"
          >
            <div>
              <input
                :value="row"
                type="text"
                inputmode="decimal"
                class="input py-2"
                :placeholder="t('admin.settings.payment.quickAmounts.amountPlaceholder')"
                :aria-label="t('admin.settings.payment.quickAmounts.amountInputLabel', { index: index + 1 })"
                :aria-invalid="Boolean(parsed.errors[index])"
                :data-testid="`payment-quick-amount-input-${index}`"
                @input="updateRow(index, $event)"
              />
              <p
                v-if="parsed.errors[index]"
                class="mt-1 text-xs text-red-600 dark:text-red-400"
                data-testid="payment-quick-amount-error"
              >{{ t(`admin.settings.payment.quickAmounts.${parsed.errors[index]}`) }}</p>
            </div>
            <button
              type="button"
              class="flex h-8 w-8 items-center justify-center rounded-lg text-gray-400 hover:bg-red-50 hover:text-red-600 dark:hover:bg-dark-700 dark:hover:text-red-400"
              :aria-label="t('admin.settings.payment.quickAmounts.remove', { index: index + 1 })"
              :title="t('admin.settings.payment.quickAmounts.remove', { index: index + 1 })"
              :data-testid="`payment-quick-amount-remove-${index}`"
              @click="removeRow(index)"
            >
              <Icon name="x" size="sm" />
            </button>
          </div>
        </div>
      </div>
      <p class="text-xs text-gray-400 dark:text-gray-500" data-testid="payment-quick-amounts-limit-hint">
        {{ t('admin.settings.payment.quickAmounts.limitHint', { min: MIN_PAYMENT_QUICK_AMOUNT, max: MAX_PAYMENT_QUICK_AMOUNT, count: MAX_PAYMENT_QUICK_AMOUNTS }) }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import Icon from '@/components/icons/Icon.vue'
import {
  defaultPaymentQuickAmounts,
  MAX_PAYMENT_QUICK_AMOUNTS,
  MAX_PAYMENT_QUICK_AMOUNT,
  MIN_PAYMENT_QUICK_AMOUNT,
  parsePaymentQuickAmountInputs,
} from '@/utils/paymentQuickAmounts'

const { t } = useI18n()

const props = defineProps<{ modelValue: string[] }>()
const emit = defineEmits<{ (event: 'update:modelValue', rows: string[]): void }>()

const rows = computed(() => props.modelValue)
const parsed = computed(() => parsePaymentQuickAmountInputs(rows.value))

function addRow() {
  if (rows.value.length >= MAX_PAYMENT_QUICK_AMOUNTS) return
  emit('update:modelValue', [...rows.value, ''])
}

function removeRow(index: number) {
  const next = [...rows.value]
  next.splice(index, 1)
  emit('update:modelValue', next)
}

function updateRow(index: number, event: Event) {
  const next = [...rows.value]
  next[index] = (event.target as HTMLInputElement).value
  emit('update:modelValue', next)
}

function resetDraft() {
  emit('update:modelValue', defaultPaymentQuickAmounts().map(String))
}
</script>
