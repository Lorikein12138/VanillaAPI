<template>
  <div class="space-y-4">
    <div v-if="!outcome && !paymentBlocked && paymentInfo" class="card space-y-2 p-4 text-center" data-test="payment-amounts">
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.actualAmount') }}</p>
      <p class="text-3xl font-semibold tabular-nums text-gray-950 dark:text-white" data-test="actual-amount">{{ currencySymbol(paymentCurrency) }}{{ paymentInfo.actual_amount }}</p>
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.amountSeparation', { ordered: displayPaymentAmount, credited: creditedAmountSymbol + creditedAmount.toFixed(2) }) }}</p>
    </div>
    <!-- ═══ Terminal States: show result, user clicks to return ═══ -->

    <!-- Success -->
    <template v-if="outcome === 'success'">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-green-100 dark:bg-green-900/30">
            <Icon name="check" size="lg" class="text-green-500" />
          </div>
          <p class="text-lg font-serif font-normal text-gray-950 dark:text-dark-50">{{ props.orderType === 'subscription' ? t('payment.result.subscriptionSuccess') : t('payment.result.success') }}</p>
          <div v-if="paidOrder" class="w-full rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
            <div class="space-y-2 text-sm">
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderId') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">#{{ paidOrder.id }}</span>
              </div>
              <div v-if="paidOrder.out_trade_no" class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderNo') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">{{ paidOrder.out_trade_no }}</span>
              </div>
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t(paidPaymentInfo ? 'payment.orders.creditedAmount' : 'payment.orders.amount') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">
                  {{ creditedAmountSymbol }}{{ paidOrder.amount.toFixed(2) }}
                  <span v-if="(paidOrder.bonus_amount ?? 0) > 0" class="ml-1 text-xs font-normal text-amber-600 dark:text-amber-400">
                    ({{ t('payment.orders.bonusIncluded', { amount: creditedAmountSymbol + (paidOrder.bonus_amount ?? 0).toFixed(2) }) }})
                  </span>
                </span>
              </div>
              <div class="flex justify-between">
                <span class="text-gray-500 dark:text-gray-400">{{ t(paidPaymentInfo ? 'payment.orders.orderedAmount' : 'payment.orders.payAmount') }}</span>
                <span class="font-medium text-gray-900 dark:text-white">{{ formatGatewayAmount(paidOrder.pay_amount, paidOrder.currency) }}</span>
              </div>
              <div v-if="paidPaymentInfo" class="flex justify-between" data-test="actual-paid-amount">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.actualPaidAmount') }}</span>
                <span class="font-semibold text-gray-900 dark:text-white">{{ currencySymbol(paidOrder.currency) }}{{ paidPaymentInfo.actual_amount }}</span>
              </div>
            </div>
          </div>
          <button class="btn btn-primary" @click="handleDone">{{ t('common.confirm') }}</button>
        </div>
      </div>
    </template>

    <!-- Cancelled -->
    <template v-else-if="outcome === 'cancelled'">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-gray-100 dark:bg-dark-700">
            <svg class="h-8 w-8 text-gray-400 dark:text-gray-500" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
              <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12" />
            </svg>
          </div>
          <p class="text-lg font-serif font-normal text-gray-950 dark:text-dark-50">{{ t('payment.qr.cancelled') }}</p>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.cancelledDesc') }}</p>
          <button class="btn btn-primary" @click="handleDone">{{ t('common.confirm') }}</button>
        </div>
      </div>
    </template>

    <!-- Expired / Failed -->
    <template v-else-if="outcome === 'expired'">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div class="flex h-16 w-16 items-center justify-center rounded-full bg-orange-100 dark:bg-orange-900/30">
            <svg class="h-8 w-8 text-orange-500" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
              <path stroke-linecap="round" stroke-linejoin="round" d="M12 6v6h4.5m4.5 0a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
          </div>
          <p class="text-lg font-serif font-normal text-gray-950 dark:text-dark-50">{{ t('payment.qr.expired') }}</p>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.expiredDesc') }}</p>
          <button class="btn btn-primary" @click="handleDone">{{ t('common.confirm') }}</button>
        </div>
      </div>
    </template>

    <!-- Recovery and local timeout require provider confirmation before payment can resume. -->
    <template v-else-if="paymentBlocked">
      <div class="card p-6 text-center space-y-4" data-test="payment-verification">
        <p class="text-sm text-gray-500 dark:text-gray-400">
          {{ verificationFailed ? t('payment.qr.verifyUnavailable') : t('payment.qr.verifying') }}
        </p>
        <button class="btn btn-secondary" :disabled="pollInFlight" @click="pollStatus(true)">
          {{ pollInFlight ? t('common.processing') : t('payment.qr.retryVerification') }}
        </button>
      </div>
    </template>

    <!-- ═══ Active States: QR or Popup waiting ═══ -->

    <!-- Mobile Alipay app handoff. The QR fallback stays hidden until launch timeout. -->
    <template v-else-if="isMobileAlipayDeepLink">
      <template v-if="!deepLinkFallbackVisible">
        <div class="card p-6">
          <div class="flex flex-col items-center space-y-4 py-4 text-center">
            <div
              v-if="deepLinkState === 'launching'"
              class="h-10 w-10 animate-spin rounded-full border-4 border-[#00AEEF] border-t-transparent"
            ></div>
            <div
              v-else
              class="flex h-12 w-12 items-center justify-center rounded-full bg-blue-50 dark:bg-blue-950/30"
            >
              <Icon name="checkCircle" size="lg" class="text-[#00AEEF]" />
            </div>
            <p class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ deepLinkState === 'backgrounded' ? t('payment.qr.alipayContinueInApp') : t('payment.qr.alipayOpening') }}
            </p>
            <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.alipayWaitingHint') }}</p>
            <button
              v-if="deepLinkState === 'backgrounded'"
              data-test="reopen-alipay"
              class="btn btn-alipay inline-flex items-center gap-2 text-sm"
              @click="reopenAlipay"
            >
              <Icon name="externalLink" size="sm" />
              {{ t('payment.qr.reopenAlipay') }}
            </button>
          </div>
        </div>
        <div class="card p-4 text-center">
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.expiresIn') }}</p>
          <p class="mt-1 text-2xl font-serif font-normal tabular-nums text-gray-950 dark:text-dark-50">{{ countdownDisplay }}</p>
          <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('payment.qr.waitingPayment') }}</p>
        </div>
      </template>
      <template v-else>
        <div data-test="alipay-qr-fallback" class="card p-6">
          <div class="flex flex-col items-center space-y-4">
            <div class="text-center">
              <p class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('payment.qr.alipayFallbackTitle') }}</p>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.alipayFallbackHint') }}</p>
            </div>
            <div class="w-full space-y-2 border-y border-gray-100 py-3 text-sm dark:border-dark-600">
              <div class="flex items-start justify-between gap-4">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.payAmount') }}</span>
                <span class="font-semibold text-gray-900 dark:text-white">{{ displayPaymentAmount }}</span>
              </div>
              <div class="flex items-start justify-between gap-4">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.orders.orderNo') }}</span>
                <span class="max-w-[70%] break-all text-right font-mono text-xs text-gray-900 dark:text-white">
                  {{ displayOrderNumber }}
                </span>
              </div>
              <div class="flex items-start justify-between gap-4">
                <span class="text-gray-500 dark:text-gray-400">{{ t('payment.qr.expiresIn') }}</span>
                <span class="font-semibold tabular-nums text-gray-900 dark:text-white">{{ countdownDisplay }}</span>
              </div>
            </div>
            <div :class="['relative rounded-lg border-2 p-4', qrBorderClass]">
              <canvas ref="qrCanvas" class="mx-auto"></canvas>
              <div class="pointer-events-none absolute inset-0 flex items-center justify-center">
                <span :class="['rounded-full p-2 shadow ring-2 ring-white', qrLogoBgClass]">
                  <img :src="qrLogoIcon" alt="" class="h-5 w-5 brightness-0 invert" />
                </span>
              </div>
            </div>
            <p class="text-center text-sm leading-6 text-gray-600 dark:text-gray-300">
              {{ t('payment.qr.alipaySaveAndScanHint') }}
            </p>
            <div class="grid w-full gap-2 sm:grid-cols-2">
              <button
                data-test="reopen-alipay"
                class="btn btn-alipay inline-flex items-center justify-center gap-2"
                @click="reopenAlipay"
              >
                <Icon name="externalLink" size="sm" />
                {{ t('payment.qr.reopenAlipay') }}
              </button>
              <button
                data-test="save-alipay-qr"
                class="btn btn-secondary inline-flex items-center justify-center gap-2"
                @click="saveQRCode"
              >
                <Icon name="download" size="sm" />
                {{ t('payment.qr.saveQRCode') }}
              </button>
            </div>
            <button class="btn btn-secondary w-full" @click="handleDone">
              {{ t('payment.result.backToRecharge') }}
            </button>
          </div>
        </div>
      </template>
    </template>

    <!-- QR Code Mode -->
    <template v-else-if="showQRCode">
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4">
          <p class="text-lg font-semibold text-gray-900 dark:text-white">{{ scanTitle }}</p>
          <div :class="['relative rounded-lg border-2 p-4', qrBorderClass]">
            <img v-if="qrImageUrl" :src="qrImageUrl" :alt="scanTitle" class="mx-auto h-[220px] w-[220px] object-contain" referrerpolicy="no-referrer" data-test="payment-qr-image" @error="qrFailed = true" />
            <canvas v-else ref="qrCanvas" class="mx-auto"></canvas>
            <!-- Brand logo overlay -->
            <div v-if="!qrImageUrl" class="pointer-events-none absolute inset-0 flex items-center justify-center">
              <span :class="['rounded-full p-2 shadow ring-2 ring-white', qrLogoBgClass]">
                <img :src="qrLogoIcon" alt="" class="h-5 w-5 brightness-0 invert" />
              </span>
            </div>
          </div>
          <p v-if="scanHint" class="text-center text-sm text-gray-500 dark:text-gray-400">{{ scanHint }}</p>
          <button v-if="cashierUrl" class="btn btn-secondary text-sm" @click="reopenPopup">
            {{ t('payment.qr.openPayWindow') }}
          </button>
        </div>
      </div>
      <div class="card p-4 text-center">
        <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.expiresIn') }}</p>
        <p class="mt-1 text-2xl font-serif font-normal tabular-nums text-gray-950 dark:text-dark-50">{{ countdownDisplay }}</p>
        <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('payment.qr.waitingPayment') }}</p>
      </div>
      <button class="btn btn-secondary w-full" :disabled="cancelling" @click="handleCancel">
        {{ cancelling ? t('common.processing') : t('payment.qr.cancelOrder') }}
      </button>
    </template>

    <!-- Waiting for Popup/Redirect Mode -->
    <template v-else>
      <div class="card p-6">
        <div class="flex flex-col items-center space-y-4 py-4">
          <div v-if="!hasPaymentInfo" class="h-10 w-10 animate-spin rounded-full border-4 border-primary-500 border-t-transparent"></div>
          <p v-if="hasPaymentInfo" data-test="qr-unavailable" class="text-center text-sm text-gray-500 dark:text-gray-400">{{ t(!cashierUrl ? 'payment.qr.unavailable' : qrExpired ? 'payment.qr.codeExpired' : 'payment.qr.cashierRequired') }}</p>
          <p v-else class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.payInNewWindowHint') }}</p>
          <button v-if="cashierUrl" class="btn btn-secondary text-sm" @click="reopenPopup">
            {{ t('payment.qr.openPayWindow') }}
          </button>
        </div>
      </div>
      <div class="card p-4 text-center">
        <p class="mt-1 text-2xl font-serif font-normal tabular-nums text-gray-950 dark:text-dark-50">{{ countdownDisplay }}</p>
        <p class="mt-1 text-xs text-gray-400 dark:text-gray-500">{{ t('payment.qr.waitingPayment') }}</p>
      </div>
      <button class="btn btn-secondary w-full" :disabled="cancelling" @click="handleCancel">
        {{ cancelling ? t('common.processing') : t('payment.qr.cancelOrder') }}
      </button>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { usePaymentStore } from '@/stores/payment'
import { useAppStore } from '@/stores'
import { paymentAPI } from '@/api/payment'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { isMobileDevice } from '@/utils/device'
import { getPaymentPopupFeatures, isBuiltInAlipayMethod, isBuiltInWxpayMethod } from '@/components/payment/providerConfig'
import { currencySymbol, formatPaymentAmount, normalizePaymentCurrency } from '@/components/payment/currency'
import type { PaymentInfo, PaymentOrder } from '@/types/payment'
import { normalizePaymentInfo, safeCashierUrl } from './paymentFlow'
import Icon from '@/components/icons/Icon.vue'
import QRCode from 'qrcode'
import alipayIcon from '@/assets/icons/alipay.svg'
import wxpayIcon from '@/assets/icons/wxpay.svg'
import paymentIcon from '@/assets/icons/payment.svg'
import {
  createAlipayDeepLinkLauncher,
  type AlipayDeepLinkLauncher,
  type AlipayDeepLinkState,
} from './alipayDeepLink'

const props = defineProps<{
  orderId: number
  amount?: number
  payAmount?: number
  qrCode: string
  expiresAt: string
  paymentType: string
  payUrl?: string
  orderType?: string
  currency?: string
  outTradeNo?: string
  mobileAlipayDeepLink?: boolean
  paymentMode?: string
  recovering?: boolean
  requireOrderRead?: boolean
  paymentInfo?: PaymentInfo | null
}>()

type PaymentOutcome = 'success' | 'cancelled' | 'expired'

const emit = defineEmits<{ done: []; success: []; settled: [outcome: PaymentOutcome]; popupOpened: [popup: Window]; orderUpdated: [order: PaymentOrder] }>()

const i18n = useI18n()
const { t } = i18n
const paymentStore = usePaymentStore()
const appStore = useAppStore()

const qrCanvas = ref<HTMLCanvasElement | null>(null)
const serverOrder = ref<PaymentOrder | null>(null)
// Once observed, a versioned contract cannot fall back to cached legacy fields.
const contractObserved = ref(props.paymentInfo !== undefined)
const hasPaymentInfo = computed(() => contractObserved.value || props.paymentInfo !== undefined || serverOrder.value?.payment_info !== undefined)
const paymentInfo = computed(() => normalizePaymentInfo(serverOrder.value
  ? (serverOrder.value.payment_info ?? (hasPaymentInfo.value ? null : undefined))
  : props.paymentInfo))
const qrFailed = ref(false)
const qrExpired = ref(false)
const qrRemainingSeconds = ref(0)
const isExplicitRedirect = computed(() => hasPaymentInfo.value && ['popup', 'redirect'].includes(props.paymentMode?.trim().toLowerCase() || ''))
const qrUrl = computed(() => hasPaymentInfo.value
  ? (!paymentInfo.value?.requires_cashier && !isExplicitRedirect.value ? paymentInfo.value?.qr_content || '' : '')
  : props.qrCode)
const qrImageUrl = computed(() => hasPaymentInfo.value && !paymentInfo.value?.requires_cashier && !isExplicitRedirect.value && !qrUrl.value
  ? paymentInfo.value?.qr_image_url || '' : '')
const cashierUrl = computed(() => {
  const url = serverOrder.value && hasPaymentInfo.value ? serverOrder.value.pay_url : props.payUrl
  return hasPaymentInfo.value ? safeCashierUrl(url) : url || ''
})
const remainingSeconds = ref(0)
const cancelling = ref(false)
const paidOrder = ref<PaymentOrder | null>(null)
const paidPaymentInfo = computed(() => normalizePaymentInfo(paidOrder.value?.payment_info))
const deepLinkState = ref<AlipayDeepLinkState>('idle')
const deepLinkFallbackVisible = ref(false)
const paymentCurrency = computed(() => normalizePaymentCurrency(serverOrder.value?.currency ?? props.currency))
const creditedAmount = computed(() => serverOrder.value?.amount ?? props.amount ?? 0)
const creditedAmountSymbol = currencySymbol('USD')
const localeCode = computed(() => {
  const raw = i18n.locale as unknown
  if (typeof raw === 'string') return raw
  if (raw && typeof raw === 'object' && 'value' in raw) {
    return String((raw as { value?: string }).value || '')
  }
  return undefined
})

// Terminal outcome: null = still active, 'success' | 'cancelled' | 'expired'
const outcome = ref<PaymentOutcome | null>(null)

let pollTimer: ReturnType<typeof setInterval> | null = null
let countdownTimer: ReturnType<typeof setInterval> | null = null
let verifyAttempts = 0
let lastVerifyAt = 0
let alipayLauncher: AlipayDeepLinkLauncher | null = null
let stopped = false

const VERIFY_RETRY_INTERVAL_MS = 15000
const VERIFY_RETRY_MAX_ATTEMPTS = 6

const isAlipay = computed(() => isBuiltInAlipayMethod(props.paymentType))
const isWxpay = computed(() => isBuiltInWxpayMethod(props.paymentType))
// qrcode is the explicit EasyPay mode. Do not infer provider scope from a visible method name.
const isQrBalance = computed(() => props.paymentMode?.trim().toLowerCase() === 'qrcode'
  && props.orderType === 'balance' && !isMobileDevice() && !props.mobileAlipayDeepLink)
const needsVerification = computed(() => isQrBalance.value || hasPaymentInfo.value)
const verificationRequired = ref(needsVerification.value && props.recovering === true)
const orderReadRequired = ref(props.recovering === true && props.requireOrderRead === true)
const verificationFailed = ref(false)
const paymentBlocked = computed(() => orderReadRequired.value || (needsVerification.value && (verificationRequired.value || (!hasPaymentInfo.value && remainingSeconds.value <= 0))))
const isMobileAlipayDeepLink = computed(() => !hasPaymentInfo.value && props.mobileAlipayDeepLink === true && isAlipay.value && !!qrUrl.value && !qrFailed.value)
const showQRCode = computed(() => !outcome.value && !paymentBlocked.value && !qrFailed.value && !qrExpired.value && !!(qrUrl.value || qrImageUrl.value)
  && (!hasPaymentInfo.value || !!cashierUrl.value)
  && (!isMobileAlipayDeepLink.value || deepLinkFallbackVisible.value))

const qrBorderClass = computed(() => {
  if (isAlipay.value) return 'border-[#00AEEF] bg-blue-50 dark:border-[#00AEEF]/70 dark:bg-blue-950/20'
  if (isWxpay.value) return 'border-[#2BB741] bg-green-50 dark:border-[#2BB741]/70 dark:bg-green-950/20'
  return 'border-gray-200 bg-white dark:border-dark-600 dark:bg-dark-800'
})

const qrLogoBgClass = computed(() => {
  if (isAlipay.value) return 'bg-[#00AEEF]'
  if (isWxpay.value) return 'bg-[#2BB741]'
  return 'bg-gray-400'
})

const qrLogoIcon = computed(() => {
  if (isAlipay.value) return alipayIcon
  if (isWxpay.value) return wxpayIcon
  return paymentIcon
})

const scanTitle = computed(() => {
  if (isAlipay.value) return t('payment.qr.scanAlipay')
  if (isWxpay.value) return t('payment.qr.scanWxpay')
  return t('payment.qr.scanToPay')
})

const scanHint = computed(() => {
  if (isAlipay.value) return t('payment.qr.scanAlipayHint')
  if (isWxpay.value) return t('payment.qr.scanWxpayHint')
  return ''
})

const countdownDisplay = computed(() => {
  const seconds = hasPaymentInfo.value && showQRCode.value ? qrRemainingSeconds.value : remainingSeconds.value
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  return m.toString().padStart(2, '0') + ':' + s.toString().padStart(2, '0')
})

const displayPaymentAmount = computed(() => formatGatewayAmount(serverOrder.value?.pay_amount ?? props.payAmount ?? props.amount ?? 0))
const displayOrderNumber = computed(() => props.outTradeNo || `#${props.orderId}`)

function formatGatewayAmount(value: number, currency?: string | null): string {
  return formatPaymentAmount(value, currency || paymentCurrency.value, localeCode.value)
}

function isSuccessStatus(status: string | null | undefined): boolean {
  return status === 'COMPLETED' || status === 'PAID' || status === 'RECHARGING'
}

function reopenPopup() {
  if (stopped || outcome.value || paymentBlocked.value) return
  if (cashierUrl.value) {
    const win = window.open(cashierUrl.value, 'paymentPopup', getPaymentPopupFeatures())
    if (!win || win.closed) {
      window.location.href = cashierUrl.value
    } else {
      emit('popupOpened', win)
    }
  }
}

function setOutcome(next: PaymentOutcome) {
  if (stopped || outcome.value) return
  outcome.value = next
  cleanup()
  emit('settled', next)
}

async function renderQR() {
  await nextTick()
  if (!showQRCode.value || !qrCanvas.value || !qrUrl.value) return
  const content = qrUrl.value
  try {
    await QRCode.toCanvas(qrCanvas.value, content, {
      width: 220, margin: 2,
      errorCorrectionLevel: 'M',
    })
  } catch {
    if (!stopped && content === qrUrl.value) qrFailed.value = true
  }
}

function updateDeepLinkState(state: AlipayDeepLinkState) {
  deepLinkState.value = state
  if (state === 'fallback') {
    deepLinkFallbackVisible.value = true
    renderQR()
  } else if (state === 'backgrounded') {
    deepLinkFallbackVisible.value = false
  }
}

function reopenAlipay() {
  alipayLauncher?.launch()
}

function saveQRCode() {
  const canvas = qrCanvas.value
  if (!canvas) return
  const link = document.createElement('a')
  link.href = canvas.toDataURL('image/png')
  link.download = `alipay-${props.outTradeNo || props.orderId}.png`
  document.body.appendChild(link)
  link.click()
  link.remove()
}

async function tryRecoverPendingOrder(order: PaymentOrder | null, force: boolean): Promise<PaymentOrder | null> {
  if (!needsVerification.value && !isWxpay.value && !isAlipay.value) return order
  const outTradeNo = String(order?.out_trade_no || props.outTradeNo || '').trim()
  if (!outTradeNo) {
    if (needsVerification.value) verificationFailed.value = true
    return order
  }
  const normalizedStatus = String(order?.status || '').trim().toUpperCase()
  if (!needsVerification.value && normalizedStatus !== 'PENDING') return order
  const now = Date.now()
  if ((!needsVerification.value && verifyAttempts >= VERIFY_RETRY_MAX_ATTEMPTS)
    || (!force && verifyAttempts > 0 && now - lastVerifyAt < VERIFY_RETRY_INTERVAL_MS)) {
    return order
  }

  lastVerifyAt = now
  verifyAttempts += 1
  const orderId = props.orderId
  try {
    const result = await paymentAPI.verifyOrder(outTradeNo)
    if (stopped || props.orderId !== orderId) return null
    if (needsVerification.value) {
      if (!result.data || result.data.id !== orderId || result.data.out_trade_no !== outTradeNo) {
        verificationFailed.value = true
        return order
      }
      verificationFailed.value = !['PENDING', ...terminalStatuses].includes(result.data.status)
      if (verificationFailed.value) return order
      if (result.data.status === 'PENDING') {
        verificationRequired.value = false
        // The server deadline can supersede an old recovery snapshot; an overdue
        // PENDING order stays blocked and is verified until its status is explicit.
        const deadline = Date.parse(result.data.expires_at)
        if (Number.isFinite(deadline)) startCountdown(Math.floor((deadline - Date.now()) / 1000))
      }
    }
    return result.data ?? order
  } catch (error: unknown) {
    if (!stopped && needsVerification.value) {
      verificationFailed.value = true
      if (typeof error === 'object' && error !== null && 'reason' in error
        && error.reason === 'PAYMENT_VERIFICATION_UNAVAILABLE') verificationRequired.value = true
    }
    return order
  }
}

const terminalStatuses = ['PAID', 'RECHARGING', 'COMPLETED', 'CANCELLED', 'EXPIRED', 'FAILED',
  'REFUND_REQUESTED', 'REFUNDING', 'REFUND_PENDING', 'PARTIALLY_REFUNDED', 'REFUNDED', 'REFUND_FAILED']

function applyServerOrder(order: PaymentOrder) {
  if (order.payment_info !== undefined && !hasPaymentInfo.value && props.recovering) verificationRequired.value = true
  if (order.payment_info !== undefined) contractObserved.value = true
  serverOrder.value = order
  orderReadRequired.value = false
  updateQrExpiry()
  emit('orderUpdated', order)
}

const pollInFlight = ref(false)
async function pollStatus(forceVerify = false) {
  if (!props.orderId || stopped || outcome.value) return
  // 防重入：接口（含 verifyOrder 二次确认）响应慢于 3 秒轮询间隔时避免并发重叠请求。
  if (pollInFlight.value) return
  pollInFlight.value = true
  const orderId = props.orderId
  try {
    let order = await paymentStore.pollOrderStatus(orderId)
    // 已进入终态则不再处理迟到的响应。
    if (stopped || props.orderId !== orderId) return
    if (order && (order.id !== orderId || (props.outTradeNo && order.out_trade_no !== props.outTradeNo))) {
      if (needsVerification.value) verificationRequired.value = true
      return
    }
    if (order) applyServerOrder(order)
    const terminal = order && terminalStatuses.includes(order.status)
    if (!terminal) order = await tryRecoverPendingOrder(order, forceVerify && needsVerification.value)
    if (stopped || props.orderId !== orderId || !order) return
    if (order.id !== orderId) return
    applyServerOrder(order)
    if (isSuccessStatus(order.status)) {
      paidOrder.value = order
      setOutcome('success')
      emit('success')
    } else if (order.status === 'CANCELLED') {
      setOutcome('cancelled')
    } else if (terminalStatuses.includes(order.status)) {
      setOutcome('expired')
    }
  } catch {
    if (!stopped && (needsVerification.value || orderReadRequired.value)) verificationFailed.value = true
  } finally {
    pollInFlight.value = false
  }
}

function updateQrExpiry() {
  if (!hasPaymentInfo.value) return
  const orderDeadline = Date.parse(serverOrder.value?.expires_at || props.expiresAt)
  const qrDeadline = Date.parse(paymentInfo.value?.expires_at || serverOrder.value?.expires_at || props.expiresAt)
  const deadline = Number.isFinite(orderDeadline) ? Math.min(orderDeadline, qrDeadline) : qrDeadline
  qrRemainingSeconds.value = Number.isFinite(deadline) ? Math.max(0, Math.ceil((deadline - Date.now()) / 1000)) : 0
  // Absolute deadline, latched for this order: a later PENDING response cannot
  // renew a QR or replace its expired deadline with a newer order deadline.
  if (qrRemainingSeconds.value <= 0 && (!props.recovering || serverOrder.value)) qrExpired.value = true
}

function startCountdown(seconds: number) {
  if (countdownTimer) { clearInterval(countdownTimer); countdownTimer = null }
  remainingSeconds.value = Math.max(0, seconds)
  const deadline = Date.now() + remainingSeconds.value * 1000
  updateQrExpiry()
  if (remainingSeconds.value <= 0) {
    if (!needsVerification.value) setOutcome('expired')
    if (!hasPaymentInfo.value) return
  }
  countdownTimer = setInterval(() => {
    remainingSeconds.value = Math.max(0, Math.floor((deadline - Date.now()) / 1000))
    updateQrExpiry()
    if (remainingSeconds.value <= 0) {
      if (needsVerification.value) {
        if (countdownTimer) clearInterval(countdownTimer)
        countdownTimer = null
        void pollStatus(true)
      } else setOutcome('expired')
    }
  }, 1000)
}

async function handleCancel() {
  if (!props.orderId || stopped || cancelling.value) return
  cancelling.value = true
  try {
    await paymentAPI.cancelOrder(props.orderId)
    setOutcome('cancelled')
  } catch (err: unknown) {
    if (stopped) return
    appStore.showError(extractI18nErrorMessage(err, t, 'payment.errors', t('common.error')))
  } finally {
    cancelling.value = false
  }
}

function handleDone() { cleanup(); emit('done') }

function cleanup() {
  stopped = true
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null }
  if (countdownTimer) { clearInterval(countdownTimer); countdownTimer = null }
  alipayLauncher?.dispose()
  alipayLauncher = null
}

// Initialize on mount
verifyAttempts = 0
lastVerifyAt = 0
let seconds = 30 * 60
if (props.expiresAt) {
  seconds = Math.floor((new Date(props.expiresAt).getTime() - Date.now()) / 1000)
}
startCountdown(seconds)
if (!stopped) pollTimer = setInterval(() => pollStatus(), 3000)
renderQR()

watch([qrUrl, qrImageUrl], () => { qrFailed.value = false })
watch([qrUrl, showQRCode], () => renderQR())
onMounted(() => {
  if (paymentBlocked.value) void pollStatus(true)
  if (!isMobileAlipayDeepLink.value) return
  alipayLauncher = createAlipayDeepLinkLauncher({
    qrCode: qrUrl.value,
    document,
    lifecycleTarget: window,
    userAgent: window.navigator.userAgent,
    assignLocation: (url) => window.location.assign(url),
    onStateChange: updateDeepLinkState,
  })
  alipayLauncher.launch()
})
onUnmounted(() => cleanup())
</script>
