<template>
  <AppLayout>
    <PaymentStatusPanel v-if="extendedOrder" class="mx-auto max-w-md py-8"
      :key="extendedOrder.id" :order-id="extendedOrder.id" :amount="extendedOrder.amount"
      :pay-amount="extendedOrder.pay_amount" :currency="extendedOrder.currency"
      :payment-info="normalizePaymentInfo(extendedOrder.payment_info) ?? null" :qr-code="''"
      :pay-url="extendedOrder.pay_url" :expires-at="extendedOrder.expires_at"
      :payment-type="extendedOrder.payment_type" :out-trade-no="extendedOrder.out_trade_no"
      :order-type="extendedOrder.order_type" :recovering="true" :require-order-read="true"
      @done="router.push('/purchase')" @success="handleSuccess" />
    <div v-else class="mx-auto flex max-w-md flex-col items-center space-y-6 py-8">
      <h2 class="text-xl font-serif font-normal text-gray-950 dark:text-white">
        {{ qrUrl ? scanTitle : t('payment.qr.payInNewWindow') }}
      </h2>
      <div v-if="qrUrl && !expired" class="rounded-2xl bg-white p-6 shadow-lg dark:bg-dark-800">
        <canvas ref="qrCanvas" class="mx-auto"></canvas>
      </div>
      <!-- Scan prompt for QR code -->
      <p v-if="qrUrl && !expired && scanHint" class="text-center text-sm text-gray-500 dark:text-gray-400">
        {{ scanHint }}
      </p>
      <div v-if="expired" class="text-center">
        <p class="text-lg font-medium text-red-500">{{ t('payment.qr.expired') }}</p>
        <button class="btn btn-primary mt-4" @click="router.push('/purchase')">{{ t('payment.result.backToRecharge') }}</button>
      </div>
      <div v-else class="text-center">
        <p v-if="informationUnavailable" data-test="payment-unavailable" class="text-sm text-gray-500 dark:text-gray-400">{{ t('payment.qr.unavailable') }}</p>
        <p v-if="!informationUnavailable" class="text-sm text-gray-500 dark:text-gray-400">{{ qrUrl ? t('payment.qr.expiresIn') : t('payment.qr.payInNewWindowHint') }}</p>
        <p class="mt-1 text-2xl font-serif font-normal tabular-nums text-gray-950 dark:text-dark-50">{{ countdownDisplay }}</p>
        <p class="mt-2 text-sm text-gray-400 dark:text-gray-500">{{ t('payment.qr.waitingPayment') }}</p>
      </div>
      <a v-if="payUrl && !qrUrl && !expired" :href="payUrl" target="_blank" rel="noopener noreferrer"
        class="btn btn-primary w-full py-3">
        {{ t('payment.qr.openPayWindow') }}
      </a>
      <!-- Cancel button -->
      <button v-if="!expired && orderId" class="btn btn-secondary w-full" :disabled="cancelling" @click="handleCancel">
        {{ cancelling ? t('common.processing') : t('payment.qr.cancelOrder') }}
      </button>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, watch, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import PaymentStatusPanel from '@/components/payment/PaymentStatusPanel.vue'
import type { PaymentOrder } from '@/types/payment'
import { PAYMENT_RECOVERY_STORAGE_KEY, normalizePaymentInfo, readPaymentRecoverySnapshot, safeCashierUrl } from '@/components/payment/paymentFlow'
import { usePaymentStore } from '@/stores/payment'
import { paymentAPI } from '@/api/payment'
import { extractI18nErrorMessage } from '@/utils/apiError'
import { useAppStore } from '@/stores'
import { isBuiltInAlipayMethod, isBuiltInWxpayMethod } from '@/components/payment/providerConfig'
import QRCode from 'qrcode'
import alipayIcon from '@/assets/icons/alipay.svg'
import wxpayIcon from '@/assets/icons/wxpay.svg'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const paymentStore = usePaymentStore()
const appStore = useAppStore()

const qrCanvas = ref<HTMLCanvasElement | null>(null)
const qrUrl = ref('')
const payUrl = ref('')
const orderId = ref(0)
const remainingSeconds = ref(0)
const expired = ref(false)
const cancelling = ref(false)
const paymentType = ref('')
const extendedOrder = ref<PaymentOrder | null>(null)
const informationUnavailable = ref(false)
let disposed = false
let terminalObserved = false
let recoveredContract = false

let pollTimer: ReturnType<typeof setInterval> | null = null
let countdownTimer: ReturnType<typeof setInterval> | null = null

const countdownDisplay = computed(() => {
  const m = Math.floor(remainingSeconds.value / 60)
  const s = remainingSeconds.value % 60
  return m.toString().padStart(2, '0') + ':' + s.toString().padStart(2, '0')
})

const isAlipay = computed(() => isBuiltInAlipayMethod(paymentType.value))
const isWxpay = computed(() => isBuiltInWxpayMethod(paymentType.value))

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

function getLogoForType(): string | null {
  if (isAlipay.value) return alipayIcon
  if (isWxpay.value) return wxpayIcon
  return null
}

async function renderQR() {
  await nextTick()
  if (!qrCanvas.value || !qrUrl.value) return

  // Use medium error correction to support logo overlay while keeping QR code scannable
  const logoSrc = getLogoForType()
  try {
    await QRCode.toCanvas(qrCanvas.value, qrUrl.value, {
      width: 256,
      margin: 2,
      errorCorrectionLevel: logoSrc ? 'M' : 'L',
    })
  } catch {
    qrUrl.value = ''
    informationUnavailable.value = !payUrl.value
    return
  }

  if (!logoSrc || disposed || expired.value || !qrCanvas.value || !qrUrl.value || extendedOrder.value) return

  // Draw logo in center of QR code
  const canvas = qrCanvas.value
  const ctx = canvas.getContext('2d')
  if (!ctx) return

  const img = new Image()
  img.src = logoSrc
  img.onload = () => {
    const logoSize = 48
    const x = (canvas.width - logoSize) / 2
    const y = (canvas.height - logoSize) / 2
    // White background with rounded corners
    const pad = 5
    ctx.fillStyle = '#FFFFFF'
    ctx.beginPath()
    const r = 6
    ctx.moveTo(x - pad + r, y - pad)
    ctx.arcTo(x + logoSize + pad, y - pad, x + logoSize + pad, y + logoSize + pad, r)
    ctx.arcTo(x + logoSize + pad, y + logoSize + pad, x - pad, y + logoSize + pad, r)
    ctx.arcTo(x - pad, y + logoSize + pad, x - pad, y - pad, r)
    ctx.arcTo(x - pad, y - pad, x + logoSize + pad, y - pad, r)
    ctx.fill()
    // Draw logo
    ctx.drawImage(img, x, y, logoSize, logoSize)
  }
}

let pollInFlight = false
async function pollStatus() {
  if (!orderId.value) return
  // 防重入：接口响应慢于 3 秒轮询间隔时避免并发重叠请求与重复跳转。
  if (pollInFlight) return
  pollInFlight = true
  try {
    const order = await paymentStore.pollOrderStatus(orderId.value)
    if (!order) return
    // 定时器已被 cleanup 清除时不再执行终态跳转（响应可能在 cleanup 后才回来）。
    if (!pollTimer) return
    applyOrder(order)
  } catch {
    // A temporary read failure never invents an order outcome.
  } finally {
    pollInFlight = false
  }
}

function handleSuccess() {
  router.push({ path: '/payment/result', query: { order_id: String(orderId.value), status: 'success' } })
}

function applyOrder(order: PaymentOrder) {
  if (disposed || terminalObserved || order.id !== orderId.value) return
  if (['COMPLETED', 'PAID', 'RECHARGING'].includes(order.status)) {
    terminalObserved = true
    qrUrl.value = ''
    payUrl.value = ''
    cleanup()
    handleSuccess()
  } else if (['EXPIRED', 'CANCELLED', 'FAILED', 'REFUND_REQUESTED', 'REFUNDING', 'REFUND_PENDING',
    'PARTIALLY_REFUNDED', 'REFUNDED', 'REFUND_FAILED'].includes(order.status)) {
    terminalObserved = true
    cleanup()
    expired.value = true
    qrUrl.value = ''
    payUrl.value = ''
  } else if (order.status === 'PENDING' && (order.payment_info !== undefined || recoveredContract)) {
    cleanup()
    qrUrl.value = ''
    payUrl.value = ''
    extendedOrder.value = order
  } else if (order.status === 'PENDING') {
    // Older servers can omit the legacy projection. Query hints are considered
    // only after an authenticated PENDING read without an extension.
    qrUrl.value = order.qr_code ?? String(route.query.qr || '')
    payUrl.value = safeCashierUrl(order.pay_url ?? route.query.pay_url)
    paymentType.value = order.payment_type || paymentType.value
    informationUnavailable.value = !qrUrl.value && !payUrl.value
    const deadline = Date.parse(order.expires_at)
    if (Number.isFinite(deadline)) {
      expired.value = false
      startCountdown(Math.floor((deadline - Date.now()) / 1000))
    }
  } else {
    qrUrl.value = ''
    payUrl.value = ''
    informationUnavailable.value = true
  }
}

function startCountdown(seconds: number) {
  if (countdownTimer) { clearInterval(countdownTimer); countdownTimer = null }
  remainingSeconds.value = Math.max(0, seconds)
  const deadline = Date.now() + remainingSeconds.value * 1000
  if (remainingSeconds.value <= 0) {
    expired.value = true
    qrUrl.value = ''
    payUrl.value = ''
    return
  }
  countdownTimer = setInterval(() => {
    remainingSeconds.value = Math.max(0, Math.floor((deadline - Date.now()) / 1000))
    if (remainingSeconds.value <= 0) {
      expired.value = true
      qrUrl.value = ''
      payUrl.value = ''
      cleanup()
    }
  }, 1000)
}

async function handleCancel() {
  if (!orderId.value || cancelling.value) return
  cancelling.value = true
  try {
    await paymentAPI.cancelOrder(orderId.value)
    terminalObserved = true
    qrUrl.value = ''
    payUrl.value = ''
    cleanup()
    router.push('/purchase')
  } catch (err: unknown) {
    appStore.showError(extractI18nErrorMessage(err, t, 'payment.errors', t('common.error')))
  } finally {
    cancelling.value = false
  }
}

function cleanup() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null }
  if (countdownTimer) { clearInterval(countdownTimer); countdownTimer = null }
}

watch(qrUrl, () => renderQR())

onMounted(async () => {
  orderId.value = Number(route.query.order_id) || 0
  const recovery = readPaymentRecoverySnapshot(localStorage.getItem(PAYMENT_RECOVERY_STORAGE_KEY))
  recoveredContract = recovery?.orderId === orderId.value && recovery?.paymentInfo !== undefined
  paymentType.value = String(route.query.payment_type || '')

  // Calculate countdown from expiresAt
  const expiresAtStr = String(route.query.expires_at || '')
  let seconds = 30 * 60 // fallback: 30 minutes
  if (expiresAtStr) {
    const expiresAt = new Date(expiresAtStr)
    const now = new Date()
    seconds = Math.floor((expiresAt.getTime() - now.getTime()) / 1000)
  }
  startCountdown(seconds)
  pollTimer = setInterval(pollStatus, 3000)
  try {
    if (!orderId.value) throw new Error('Missing order ID')
    const result = await paymentAPI.getOrder(orderId.value)
    if (!disposed) applyOrder(result.data)
  } catch {
    if (!disposed) informationUnavailable.value = true
  }
})

onUnmounted(() => { disposed = true; cleanup() })
</script>
