import type {
  CreateOrderRequest,
  CreateOrderResult,
  MethodLimit,
  OrderType,
  PaymentInfo,
  WechatJSAPIPayload,
  WechatOAuthInfo,
} from '@/types/payment'

export const PAYMENT_RECOVERY_STORAGE_KEY = 'payment.recovery.current'

const VISIBLE_METHOD_ALIASES = {
  alipay: 'alipay',
  alipay_direct: 'alipay',
  wxpay: 'wxpay',
  wxpay_direct: 'wxpay',
  stripe: 'stripe',
  airwallex: 'airwallex',
} as const

export type VisiblePaymentMethod = 'alipay' | 'wxpay' | 'stripe' | 'airwallex'
export type StripeVisibleMethod = 'alipay' | 'wechat_pay'
export type PaymentLaunchKind =
  | 'qr_waiting'
  | 'alipay_deep_link'
  | 'redirect_waiting'
  | 'stripe_popup'
  | 'stripe_route'
  | 'airwallex_route'
  | 'wechat_oauth'
  | 'wechat_jsapi'
  | 'unhandled'

export interface PaymentRecoverySnapshot {
  orderId: number
  amount: number
  qrCode: string
  expiresAt: string
  paymentType: string
  payUrl: string
  outTradeNo: string
  clientSecret: string
  intentId: string
  currency: string
  countryCode: string
  paymentEnv: string
  payAmount: number
  orderType: OrderType | ''
  paymentMode: string
  resumeToken: string
  alipayMobilePrecreateDeepLink?: boolean
  /** null preserves a present but invalid/withdrawn contract and blocks legacy QR. */
  paymentInfo?: PaymentInfo | null
  createdAt: number
}

export interface PaymentLaunchContext {
  visibleMethod: string
  orderType: OrderType
  isMobile: boolean
  isWechatBrowser?: boolean
  /** When true, Alipay payments always use QR code regardless of device type */
  forceQRCode?: boolean
  /** When true, the new mobile Alipay precreate flow takes priority over forceQRCode */
  mobilePrecreateDeepLink?: boolean
  now?: number
  stripePopupUrl?: string
  stripeRouteUrl?: string
  airwallexRouteUrl?: string
}

export interface PaymentLaunchDecision {
  kind: PaymentLaunchKind
  paymentState: PaymentRecoverySnapshot
  recovery: PaymentRecoverySnapshot
  stripeMethod?: StripeVisibleMethod
  oauth?: WechatOAuthInfo
  jsapi?: WechatJSAPIPayload
}

export interface BuildCreateOrderPayloadInput {
  amount: number
  paymentType: string
  orderType: OrderType
  planId?: number
  origin?: string
  isMobile: boolean
  isWechatBrowser: boolean
  /** When true, Alipay payments always use QR code (passes is_mobile: false to backend) */
  forceQRCode?: boolean
  /** When true, keep the real mobile signal so the backend can select precreate */
  mobilePrecreateDeepLink?: boolean
}

type CreateOrderFlowResult = CreateOrderResult & {
  resume_token?: string
}

type StorageWriter = Pick<Storage, 'removeItem' | 'setItem'>

export function safeCashierUrl(value: unknown): string {
  if (typeof value !== 'string' || !/^https?:\/\/[^/]/i.test(value)
    || value !== value.trim() || /[\\\s\p{Cc}\p{Cs}]/u.test(value)) return ''
  try {
    if (/[\\\p{Cc}\p{Cs}]/u.test(decodeURIComponent(value))) return ''
    const url = new URL(value)
    return ['http:', 'https:'].includes(url.protocol) && !!url.hostname && !url.username && !url.password ? value : ''
  } catch {
    return ''
  }
}

function validPaymentInfoExpiry(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const parts = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:Z|([+-])(\d{2}):(\d{2}))$/.exec(value)
  if (!parts) return false
  const [, year, month, day, hour, minute, second, , offsetHour, offsetMinute] = parts
  const y = Number(year)
  const m = Number(month)
  const days = [31, y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  return m >= 1 && m <= 12 && Number(day) >= 1 && Number(day) <= days[m - 1]!
    && Number(hour) < 24 && Number(minute) < 60 && Number(second) < 60
    && Number(offsetHour || 0) < 24 && Number(offsetMinute || 0) < 60 && Number.isFinite(Date.parse(value))
}

export function actualPaymentLabelKey(status: string): string {
  return ['PAID', 'RECHARGING', 'COMPLETED', 'REFUND_REQUESTED', 'REFUNDING', 'REFUND_PENDING',
    'PARTIALLY_REFUNDED', 'REFUNDED', 'REFUND_FAILED'].includes(status)
    ? 'payment.orders.actualPaidAmount' : 'payment.orders.actualPayAmount'
}

/** undefined means legacy; null means fail closed without consulting legacy qr_code. */
export function normalizePaymentInfo(value: unknown): PaymentInfo | null | undefined {
  if (value === undefined) return undefined
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  const info = value as Record<string, unknown>
  if (info.version !== 1 || typeof info.actual_amount !== 'string'
    || !/^(?:0|[1-9]\d{0,11})(?:\.\d{1,2})?$/.test(info.actual_amount)
    || !/[1-9]/.test(info.actual_amount)
    || (info.requires_cashier !== undefined && typeof info.requires_cashier !== 'boolean')) return null
  if (info.qr_content !== undefined && (typeof info.qr_content !== 'string' || !info.qr_content.trim()
    || /[\p{Cc}\p{Cs}]/u.test(info.qr_content)
    || /^\s*(?:<|(?:javascript|vbscript|data|file|blob):)/i.test(info.qr_content))) return null
  if (info.qr_content === undefined && info.qr_image_url !== undefined && !safeCashierUrl(info.qr_image_url)) return null
  if (info.expires_at !== undefined && !validPaymentInfoExpiry(info.expires_at)) return null
  return {
    version: 1,
    actual_amount: info.actual_amount,
    ...(info.qr_content !== undefined ? { qr_content: info.qr_content as string } : {}),
    ...(safeCashierUrl(info.qr_image_url) ? { qr_image_url: info.qr_image_url as string } : {}),
    ...(info.expires_at !== undefined ? { expires_at: info.expires_at as string } : {}),
    ...(info.requires_cashier !== undefined ? { requires_cashier: info.requires_cashier as boolean } : {}),
  }
}

export function normalizeVisibleMethod(method: string): VisiblePaymentMethod | '' {
  const normalized = VISIBLE_METHOD_ALIASES[method.trim() as keyof typeof VISIBLE_METHOD_ALIASES]
  return normalized ?? ''
}

export function getVisibleMethods(methods: Record<string, MethodLimit>): Record<string, MethodLimit> {
  const visible: Record<string, MethodLimit> = {}

  Object.entries(methods).forEach(([type, limit]) => {
    const normalized = normalizeVisibleMethod(type) || type.trim()
    if (!normalized) return

    const isCanonical = type === normalized
    const existing = visible[normalized]
    if (!existing || isCanonical) {
      visible[normalized] = { ...limit }
    }
  })

  return visible
}

export function buildCreateOrderPayload(input: BuildCreateOrderPayloadInput): CreateOrderRequest {
  const visibleMethod = normalizeVisibleMethod(input.paymentType) || input.paymentType.trim()
  const normalizedOrigin = (input.origin || '').trim().replace(/\/+$/, '')
  // When forceQRCode is enabled for alipay, always tell the backend this is not a mobile
  // request so it generates a QR code instead of a mobile-redirect URL.
  const effectiveMobile = (input.forceQRCode && !input.mobilePrecreateDeepLink && visibleMethod === 'alipay')
    ? false
    : input.isMobile
  const payload: CreateOrderRequest = {
    amount: input.amount,
    payment_type: visibleMethod,
    order_type: input.orderType,
    is_mobile: effectiveMobile,
    payment_source: visibleMethod === 'wxpay' && input.isWechatBrowser
      ? 'wechat_in_app_resume'
      : 'hosted_redirect',
  }

  if (input.planId) {
    payload.plan_id = input.planId
  }
  if (normalizedOrigin) {
    payload.return_url = `${normalizedOrigin}/payment/result`
  }

  return payload
}

export function decidePaymentLaunch(
  result: CreateOrderFlowResult,
  context: PaymentLaunchContext,
): PaymentLaunchDecision {
  const visibleMethod = normalizeVisibleMethod(context.visibleMethod) || context.visibleMethod
  const baseState = createPaymentRecoverySnapshot({
    orderId: result.order_id,
    amount: result.amount,
    qrCode: result.qr_code || '',
    expiresAt: result.expires_at || '',
    paymentType: visibleMethod,
    payUrl: result.pay_url || '',
    outTradeNo: result.out_trade_no || '',
    clientSecret: result.client_secret || '',
    intentId: result.intent_id || '',
    currency: result.currency || '',
    countryCode: result.country_code || '',
    paymentEnv: result.payment_env || '',
    payAmount: result.pay_amount,
    orderType: context.orderType,
    paymentMode: (result.payment_mode || '').trim(),
    resumeToken: result.resume_token || '',
    alipayMobilePrecreateDeepLink: result.alipay_mobile_precreate_deep_link === true,
    paymentInfo: normalizePaymentInfo(result.payment_info),
  }, context.now)

  if (visibleMethod === 'airwallex' && baseState.clientSecret && baseState.intentId) {
    if (!context.airwallexRouteUrl) {
      return { kind: 'unhandled', paymentState: baseState, recovery: baseState }
    }
    const paymentState = { ...baseState, payUrl: context.airwallexRouteUrl || '' }
    return { kind: 'airwallex_route', paymentState, recovery: paymentState }
  }

  if (baseState.clientSecret) {
    // visibleMethod === 'stripe' means the user clicked the dedicated Stripe button
    // and should land on the full Payment Element to choose a sub-method themselves.
    const isStripeButton = visibleMethod === 'stripe'
    const stripeMethod: StripeVisibleMethod | undefined = isStripeButton
      ? undefined
      : visibleMethod === 'wxpay' ? 'wechat_pay' : 'alipay'
    const kind: PaymentLaunchKind = stripeMethod === 'alipay' && !context.isMobile
      ? 'stripe_popup'
      : 'stripe_route'
    const payUrl = kind === 'stripe_popup'
      ? context.stripePopupUrl || context.stripeRouteUrl || ''
      : context.stripeRouteUrl || context.stripePopupUrl || ''
    const paymentState = { ...baseState, payUrl }
    return { kind, paymentState, recovery: paymentState, stripeMethod }
  }

  if (result.result_type === 'oauth_required' && result.oauth?.authorize_url) {
    return { kind: 'wechat_oauth', paymentState: baseState, recovery: baseState, oauth: result.oauth }
  }

  const jsapiPayload = result.jsapi ?? result.jsapi_payload
  if (result.result_type === 'jsapi_ready' && jsapiPayload) {
    return { kind: 'wechat_jsapi', paymentState: baseState, recovery: baseState, jsapi: jsapiPayload }
  }

  // The versioned contract owns QR selection. Keep explicit popup/redirect modes.
  if (baseState.paymentInfo !== undefined) {
    baseState.qrCode = ''
    baseState.payUrl = safeCashierUrl(baseState.payUrl)
    baseState.alipayMobilePrecreateDeepLink = false
    const info = baseState.paymentInfo
    const qrAvailable = info && !info.requires_cashier && (info.qr_content || info.qr_image_url)
      && (!info.expires_at || Date.parse(info.expires_at) > (context.now ?? Date.now()))
    const redirect = ['popup', 'redirect'].includes(baseState.paymentMode.toLowerCase())
    return {
      kind: baseState.payUrl && (redirect || !qrAvailable) ? 'redirect_waiting' : 'qr_waiting',
      paymentState: baseState,
      recovery: baseState,
    }
  }

  if (
    visibleMethod === 'alipay'
    && context.isMobile
    && baseState.alipayMobilePrecreateDeepLink
    && baseState.qrCode
  ) {
    return { kind: 'alipay_deep_link', paymentState: baseState, recovery: baseState }
  }

  const normalizedPaymentMode = baseState.paymentMode.trim().toLowerCase()
  // When forceQRCode is on for alipay, treat the device as desktop so the mobile-redirect
  // branch is bypassed and we fall through to qr_waiting.
  const effectiveMobile = (context.forceQRCode && !context.mobilePrecreateDeepLink && visibleMethod === 'alipay')
    ? false
    : context.isMobile
  const prefersRedirect = normalizedPaymentMode === 'redirect'
    || normalizedPaymentMode === 'popup'
    || (effectiveMobile && !!baseState.payUrl)
  const prefersQr = normalizedPaymentMode === 'qrcode'
    || normalizedPaymentMode === 'native'
    || (!prefersRedirect && !!baseState.qrCode)

  if (visibleMethod === 'wxpay' && context.isWechatBrowser && baseState.payUrl && !baseState.qrCode) {
    return { kind: 'redirect_waiting', paymentState: baseState, recovery: baseState }
  }

  if (prefersRedirect && baseState.payUrl) {
    return { kind: 'redirect_waiting', paymentState: baseState, recovery: baseState }
  }

  if (prefersQr && baseState.qrCode) {
    return { kind: 'qr_waiting', paymentState: baseState, recovery: baseState }
  }

  if (baseState.payUrl) {
    return { kind: 'redirect_waiting', paymentState: baseState, recovery: baseState }
  }

  return { kind: 'unhandled', paymentState: baseState, recovery: baseState }
}

export function createPaymentRecoverySnapshot(
  state: Omit<PaymentRecoverySnapshot, 'createdAt'>,
  now = Date.now(),
): PaymentRecoverySnapshot {
  return {
    ...state,
    createdAt: now,
  }
}

export function writePaymentRecoverySnapshot(
  storage: StorageWriter,
  snapshot: PaymentRecoverySnapshot,
  key = PAYMENT_RECOVERY_STORAGE_KEY,
): void {
  storage.setItem(key, JSON.stringify(snapshot))
}

export function clearPaymentRecoverySnapshot(
  storage: Pick<Storage, 'removeItem'>,
  key = PAYMENT_RECOVERY_STORAGE_KEY,
): void {
  storage.removeItem(key)
}

export function readPaymentRecoverySnapshot(
  raw: string | null | undefined,
  options: { now?: number; resumeToken?: string; retainExpiredQrBalance?: boolean } = {},
): PaymentRecoverySnapshot | null {
  if (!raw) return null

  try {
    const parsed = JSON.parse(raw) as Partial<PaymentRecoverySnapshot>
    if (
      typeof parsed.orderId !== 'number'
      || typeof parsed.amount !== 'number'
      || typeof parsed.qrCode !== 'string'
      || typeof parsed.expiresAt !== 'string'
      || typeof parsed.paymentType !== 'string'
      || typeof parsed.payUrl !== 'string'
      || (parsed.outTradeNo != null && typeof parsed.outTradeNo !== 'string')
      || typeof parsed.clientSecret !== 'string'
      || (parsed.intentId != null && typeof parsed.intentId !== 'string')
      || (parsed.currency != null && typeof parsed.currency !== 'string')
      || (parsed.countryCode != null && typeof parsed.countryCode !== 'string')
      || (parsed.paymentEnv != null && typeof parsed.paymentEnv !== 'string')
      || typeof parsed.payAmount !== 'number'
      || typeof parsed.paymentMode !== 'string'
      || typeof parsed.resumeToken !== 'string'
      || (parsed.alipayMobilePrecreateDeepLink != null && typeof parsed.alipayMobilePrecreateDeepLink !== 'boolean')
      || typeof parsed.createdAt !== 'number'
    ) {
      return null
    }

    const now = options.now ?? Date.now()
    const expiresAt = Date.parse(parsed.expiresAt)
    const retainExpired = options.retainExpiredQrBalance && parsed.orderType === 'balance'
      && parsed.paymentMode.trim().toLowerCase() === 'qrcode' && !parsed.alipayMobilePrecreateDeepLink
    if (Number.isFinite(expiresAt) && expiresAt <= now && !retainExpired && parsed.paymentInfo === undefined) {
      return null
    }
    if (options.resumeToken && parsed.resumeToken !== options.resumeToken) {
      return null
    }

    return {
      orderId: parsed.orderId,
      amount: parsed.amount,
      qrCode: parsed.paymentInfo !== undefined ? '' : parsed.qrCode,
      expiresAt: parsed.expiresAt,
      paymentType: parsed.paymentType,
      payUrl: parsed.paymentInfo !== undefined ? safeCashierUrl(parsed.payUrl) : parsed.payUrl,
      outTradeNo: parsed.outTradeNo || '',
      clientSecret: parsed.clientSecret,
      intentId: parsed.intentId || '',
      currency: parsed.currency || '',
      countryCode: parsed.countryCode || '',
      paymentEnv: parsed.paymentEnv || '',
      payAmount: parsed.payAmount,
      orderType: parsed.orderType === 'subscription' ? 'subscription' : 'balance',
      paymentMode: parsed.paymentMode,
      resumeToken: parsed.resumeToken,
      alipayMobilePrecreateDeepLink: parsed.alipayMobilePrecreateDeepLink === true,
      paymentInfo: normalizePaymentInfo(parsed.paymentInfo),
      createdAt: parsed.createdAt,
    }
  } catch {
    return null
  }
}
