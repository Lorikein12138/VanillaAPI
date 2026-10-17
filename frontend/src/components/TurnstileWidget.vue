<template>
  <div v-if="siteKey" ref="wrapperRef" class="turnstile-wrapper">
    <div class="turnstile-stage" :class="stageClass">
      <div
        v-if="loading"
        role="status"
        class="absolute inset-0 flex items-center justify-center rounded-lg border border-gray-200 bg-gray-50 text-sm text-gray-500 dark:border-dark-600 dark:bg-dark-700 dark:text-gray-400"
      >
        {{ t('auth.captchaLoading') }}
      </div>
      <div ref="containerRef" class="turnstile-container"></div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

interface TurnstileRenderOptions {
  sitekey: string
  callback: (token: string) => void
  'expired-callback'?: () => void
  'error-callback'?: () => void
  theme?: 'light' | 'dark' | 'auto'
  size?: 'normal' | 'compact' | 'flexible'
}

interface TurnstileAPI {
  render: (container: HTMLElement, options: TurnstileRenderOptions) => string
  reset: (widgetId?: string) => void
  remove: (widgetId?: string) => void
}

declare global {
  interface Window {
    turnstile?: TurnstileAPI
    onTurnstileLoad?: () => void
  }
}

type PreferredWidgetSize = 'normal' | 'compact' | 'flexible'
type EffectiveWidgetSize = PreferredWidgetSize | null

const props = withDefaults(
  defineProps<{
    siteKey: string
    theme?: 'light' | 'dark' | 'auto'
    size?: PreferredWidgetSize
  }>(),
  {
    theme: 'auto',
    size: 'normal'
  }
)

const emit = defineEmits<{
  (e: 'verify', token: string): void
  (e: 'expire'): void
  (e: 'error'): void
}>()

const { t } = useI18n()
const loading = ref(true)
const wrapperRef = ref<HTMLElement | null>(null)
const containerRef = ref<HTMLElement | null>(null)
const widgetId = ref<string | null>(null)
const availableWidth = ref<number | null>(null)
const renderedSize = ref<EffectiveWidgetSize>(null)
const scriptLoaded = ref(false)

let resizeObserver: ResizeObserver | null = null
let fallbackResizeListenerActive = false
let initialization: Promise<void> | null = null
let mounted = false
let generation = 0

function resolveWidgetSize(width: number | null, preferred: PreferredWidgetSize): EffectiveWidgetSize {
  if (width === null || width <= 0) return null
  if (preferred === 'compact' || width < 300) return 'compact'
  return preferred
}

const effectiveSize = computed(() => resolveWidgetSize(availableWidth.value, props.size))
const stageClass = computed(() => `turnstile-stage--${effectiveSize.value ?? 'unmeasured'}`)

const loadScript = (): Promise<void> => {
  return new Promise((resolve, reject) => {
    if (window.turnstile) {
      scriptLoaded.value = true
      resolve()
      return
    }

    // Check if script is already loading
    const existingScript = document.querySelector('script[src*="turnstile"]')
    if (existingScript) {
      window.onTurnstileLoad = () => {
        scriptLoaded.value = true
        resolve()
      }
      return
    }

    const script = document.createElement('script')
    script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?onload=onTurnstileLoad'
    script.async = true
    script.defer = true

    window.onTurnstileLoad = () => {
      scriptLoaded.value = true
      resolve()
    }

    script.onerror = () => {
      reject(new Error('Failed to load Turnstile script'))
    }

    document.head.appendChild(script)
  })
}

function measureWrapper() {
  const width = wrapperRef.value?.getBoundingClientRect().width ?? 0
  updateAvailableWidth(width)
}

function updateAvailableWidth(width: number) {
  availableWidth.value = Number.isFinite(width) && width > 0 ? width : 0
  reconcileWidget()
}

function removeWidget(id: string) {
  try {
    window.turnstile?.remove(id)
  } catch {
    // Ignore errors when removing a widget whose SDK instance has already expired.
  }
}

function disposeWidget(notifyExpire: boolean) {
  const previousId = widgetId.value
  generation += 1
  widgetId.value = null
  renderedSize.value = null

  if (!previousId) return
  if (notifyExpire) emit('expire')
  removeWidget(previousId)
}

function renderWidget(size: EffectiveWidgetSize) {
  const sdk = window.turnstile
  const container = containerRef.value
  if (!sdk || !container || !props.siteKey || !size || !mounted) return

  container.innerHTML = ''
  const callbackGeneration = ++generation
  const isCurrentGeneration = () => mounted && callbackGeneration === generation

  try {
    const nextWidgetId = sdk.render(container, {
      sitekey: props.siteKey,
      callback: (token: string) => {
        if (isCurrentGeneration()) emit('verify', token)
      },
      'expired-callback': () => {
        if (isCurrentGeneration()) emit('expire')
      },
      'error-callback': () => {
        if (!isCurrentGeneration()) return
        loading.value = false
        emit('error')
      },
      theme: props.theme,
      size
    })
    widgetId.value = nextWidgetId
    renderedSize.value = size
    loading.value = false
  } catch {
    generation += 1
    widgetId.value = null
    renderedSize.value = null
    loading.value = false
    emit('error')
  }
}

function reconcileWidget(forceReplace = false) {
  if (!mounted || !scriptLoaded.value || !window.turnstile) return
  const nextSize = effectiveSize.value
  if (!nextSize || !containerRef.value) return

  if (widgetId.value && !forceReplace && renderedSize.value === nextSize) return

  if (widgetId.value) {
    const sizeChanged = renderedSize.value !== nextSize
    disposeWidget(sizeChanged)
  }

  renderWidget(nextSize)
}

function reset() {
  if (window.turnstile && widgetId.value) {
    window.turnstile.reset(widgetId.value)
  }
}

async function initialize() {
  if (!mounted || !props.siteKey) return
  if (!initialization) {
    initialization = loadScript()
      .then(() => {
        scriptLoaded.value = true
      })
      .catch((error: unknown) => {
        if (mounted) {
          console.error('Failed to initialize Turnstile:', error)
          loading.value = false
          emit('error')
        }
      })
      .finally(() => {
        initialization = null
      })
  }
  await initialization
  if (mounted) reconcileWidget()
}

function startObserving() {
  if (!props.siteKey || resizeObserver || fallbackResizeListenerActive) return
  measureWrapper()
  if (typeof ResizeObserver !== 'undefined' && wrapperRef.value) {
    resizeObserver = new ResizeObserver(entries => {
      const entry = entries.find(candidate => candidate.target === wrapperRef.value)
      if (entry) updateAvailableWidth(entry.contentRect.width)
    })
    resizeObserver.observe(wrapperRef.value)
    return
  }
  window.addEventListener('resize', measureWrapper)
  fallbackResizeListenerActive = true
}

// Expose reset method to parent.
defineExpose({ reset })

onMounted(() => {
  mounted = true
  if (!props.siteKey) return
  startObserving()
  void initialize()
})

onUnmounted(() => {
  mounted = false
  generation += 1
  resizeObserver?.disconnect()
  resizeObserver = null
  window.removeEventListener('resize', measureWrapper)
  fallbackResizeListenerActive = false
  disposeWidget(false)
})

watch(
  () => props.size,
  () => reconcileWidget()
)

watch(
  () => props.siteKey,
  (newKey, oldKey) => {
    if (newKey !== oldKey && widgetId.value) disposeWidget(false)
    if (!newKey) return
    startObserving()
    if (scriptLoaded.value) reconcileWidget(true)
    else void initialize()
  },
  { flush: 'post' }
)
</script>

<style scoped>
.turnstile-wrapper {
  display: flex;
  justify-content: center;
  width: 100%;
  min-width: 0;
}

.turnstile-stage {
  position: relative;
  flex: 0 0 auto;
}

.turnstile-stage--unmeasured {
  width: 100%;
  min-width: 0;
  min-height: 65px;
}

.turnstile-stage--normal {
  width: 300px;
  min-height: 65px;
}

.turnstile-stage--compact {
  width: 150px;
  min-height: 140px;
}

.turnstile-stage--flexible {
  width: 100%;
  min-width: 0;
  min-height: 65px;
}

.turnstile-container {
  width: 100%;
  min-height: 65px;
}
</style>
