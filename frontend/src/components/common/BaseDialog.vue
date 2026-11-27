<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="show"
        class="modal-overlay"
        :style="zIndexStyle"
        :aria-labelledby="dialogId"
        role="dialog"
        aria-modal="true"
        @click.self="handleClose"
      >
        <!-- Modal panel -->
        <div ref="dialogRef" :class="['modal-content', widthClasses]" tabindex="-1" @click.stop>
          <!-- Header -->
          <div class="modal-header">
            <h3 :id="dialogId" class="modal-title">
              {{ title }}
            </h3>
            <button
              v-if="showCloseButton"
              @click="emit('close')"
              class="-mr-2 rounded-xl p-2 text-gray-400 transition-colors hover:bg-gray-100 hover:text-gray-600 focus:outline-none focus-visible:ring-2 focus-visible:ring-primary-500/30 focus-visible:ring-offset-2 dark:text-dark-500 dark:hover:bg-dark-700 dark:hover:text-dark-300 dark:focus-visible:ring-offset-dark-900"
              :aria-label="t('common.closeDialog')"
            >
              <Icon name="x" size="md" />
            </button>
          </div>

          <!-- Body -->
          <div ref="modalBodyRef" class="modal-body">
            <slot></slot>
          </div>

          <!-- Footer -->
          <div v-if="$slots.footer" class="modal-footer">
            <slot name="footer"></slot>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<script lang="ts">
let dialogIdCounter = 0
interface OpenDialog {
  zIndex: () => number
  panel: () => HTMLElement | null
  opener: HTMLElement | null
}
const openDialogs = new Map<string, OpenDialog>()

function topDialog() {
  let top: OpenDialog | undefined
  for (const dialog of openDialogs.values()) {
    if (!top || dialog.zIndex() > top.zIndex()) {
      top = dialog
    } else if (dialog.zIndex() === top.zIndex()) {
      const previous = top.panel()
      const current = dialog.panel()
      // Equal z-index paints in the teleported DOM order.
      if (!previous || (current && (previous.compareDocumentPosition(current) & Node.DOCUMENT_POSITION_FOLLOWING))) {
        top = dialog
      }
    }
  }
  return top
}
</script>

<script setup lang="ts">
import { computed, watch, onMounted, onBeforeUnmount, ref, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { containTab, getTabStops } from '@/utils/tabStops'

const { t } = useI18n()
// 生成唯一ID以避免多个对话框时ID冲突
const dialogId = `modal-title-${++dialogIdCounter}`

// 焦点管理
const dialogRef = ref<HTMLElement | null>(null)
const modalBodyRef = ref<HTMLElement | null>(null)

type DialogWidth = 'narrow' | 'normal' | 'wide' | 'extra-wide' | 'full'

interface Props {
  show: boolean
  title: string
  width?: DialogWidth
  closeOnEscape?: boolean
  closeOnClickOutside?: boolean
  showCloseButton?: boolean
  zIndex?: number
}

interface Emits {
  (e: 'close'): void
}

const props = withDefaults(defineProps<Props>(), {
  width: 'normal',
  closeOnEscape: true,
  closeOnClickOutside: false,
  showCloseButton: true,
  zIndex: 50
})

const emit = defineEmits<Emits>()
const dialog: OpenDialog = { zIndex: () => props.zIndex, panel: () => dialogRef.value, opener: null }

// Custom z-index style (overrides the default z-50 from CSS)
const zIndexStyle = computed(() => {
  return props.zIndex !== 50 ? { zIndex: props.zIndex } : undefined
})

const widthClasses = computed(() => {
  // Width guidance: narrow=confirm/short prompts, normal=standard forms,
  // wide=multi-section forms or rich content, extra-wide=analytics/tables,
  // full=full-screen or very dense layouts.
  const widths: Record<DialogWidth, string> = {
    narrow: 'max-w-md',
    normal: 'max-w-lg',
    wide: 'w-full sm:max-w-2xl md:max-w-3xl lg:max-w-4xl',
    'extra-wide': 'w-full sm:max-w-3xl md:max-w-4xl lg:max-w-5xl xl:max-w-6xl',
    full: 'w-full sm:max-w-4xl md:max-w-5xl lg:max-w-6xl xl:max-w-7xl'
  }
  return widths[props.width]
})

const handleClose = () => {
  if (isTopDialog() && props.closeOnClickOutside) {
    emit('close')
  }
}

const isTopDialog = () => topDialog() === dialog

const handleKeydown = (event: KeyboardEvent) => {
  const dialog = dialogRef.value
  if (!dialog || !props.show || !isTopDialog() || event.defaultPrevented) return
  if (props.closeOnEscape && event.key === 'Escape') {
    event.preventDefault()
    emit('close')
    return
  }
  if (event.key === 'Tab') containTab(event, dialog)
}

const updateScrollLock = (isOpen: boolean) => {
  if (isOpen) openDialogs.set(dialogId, dialog)
  else openDialogs.delete(dialogId)
  document.body.classList.toggle('modal-open', openDialogs.size > 0)
}

function closeDialog() {
  const wasTop = isTopDialog()
  const opener = dialog.opener
  const closingPanel = dialog.panel()
  dialog.opener = null
  updateScrollLock(false)
  for (const remaining of openDialogs.values()) {
    if (closingPanel?.contains(remaining.opener)) remaining.opener = opener
  }
  if (!wasTop) return
  const next = topDialog()?.panel()
  if (opener?.isConnected && !opener.closest('[inert]') && (!next || next.contains(opener))) {
    opener.focus()
  } else if (next) {
    focusPanel(next)
  }
}

function focusPanel(panel: HTMLElement) {
  const target = getTabStops(panel)[0] ?? panel
  target.focus()
}

watch(() => props.zIndex, async () => {
  await nextTick()
  const panel = topDialog()?.panel()
  if (panel && !panel.contains(document.activeElement)) focusPanel(panel)
})

// Prevent body scroll when modal is open and manage focus
watch(
  () => props.show,
  async (isOpen) => {
    if (isOpen) {
      // 保存当前焦点元素
      dialog.opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
      // 使用CSS类而不是直接操作style,更易于管理多个对话框
      updateScrollLock(true)

      // 等待DOM更新后设置焦点到对话框
      await nextTick()
      if (!isTopDialog()) return
      if (modalBodyRef.value) {
        modalBodyRef.value.scrollTop = 0
      }
      if (dialogRef.value) {
        focusPanel(dialogRef.value)
      }
    } else {
      closeDialog()
    }
  },
  { immediate: true }
)

onMounted(() => {
  document.addEventListener('keydown', handleKeydown)
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', handleKeydown)
  closeDialog()
})
</script>
