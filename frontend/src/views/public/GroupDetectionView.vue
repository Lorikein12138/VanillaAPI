<template>
  <AppLayout>
    <section class="mx-auto max-w-5xl space-y-4">
      <h1 class="page-title lg:hidden">{{ t('channelDetection.publicTitle') }}</h1>
      <p v-if="loading" role="status" class="card p-5 text-sm text-gray-600 dark:text-dark-300">{{ t('common.loading') }}</p>
      <div v-else-if="errorKey" class="card space-y-3 p-5">
        <p role="status">{{ t(errorKey) }}</p>
        <button v-if="errorKey === 'channelDetection.loadError'" type="button" class="btn btn-secondary" @click="load()">{{ t('channelDetection.retry') }}</button>
      </div>
      <p v-else-if="!groups.length" class="card p-5 text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.empty') }}</p>
      <div v-else class="grid grid-cols-[repeat(auto-fill,minmax(min(100%,19rem),1fr))] gap-4">
        <article v-for="group in groups" :key="group.group_id ?? group.name" class="card min-w-0 self-start overflow-hidden">
          <div class="grid aspect-square min-w-0 grid-rows-[auto_minmax(0,1fr)] gap-2.5 p-3 sm:p-4">
            <header class="min-w-0">
              <h2 :title="group.name" class="truncate font-serif text-xl font-normal leading-6 text-gray-950 dark:text-dark-50">{{ group.name }}</h2>
              <div class="flex min-w-0 items-baseline justify-between gap-2 text-xs leading-4 text-gray-600 dark:text-dark-300">
                <p :title="group.model" class="min-w-0 truncate">{{ group.model }}</p>
                <time v-if="group.sample" :datetime="group.sample.checked_at" class="shrink-0 text-gray-500 dark:text-dark-400">
                  {{ formatTime(group.sample.checked_at) }}
                </time>
              </div>
            </header>
            <template v-if="group.sample">
              <figure class="grid min-h-0 min-w-0 grid-rows-[auto_minmax(0,1fr)] gap-1">
                <figcaption class="text-xs font-medium leading-4 text-gray-700 dark:text-dark-200">{{ t('channelDetection.pelican') }}</figcaption>
                <div class="detection-preview grid min-h-0 min-w-0 place-items-center">
                  <img
                    v-if="group.sample.pelican_status === 'generated' && group.sample.svg"
                    :src="imageSource(group.sample.svg)"
                    :alt="t('channelDetection.pelicanAlt')"
                    class="detection-art rounded-lg border border-gray-200 bg-[#fbfaf7] object-contain dark:border-dark-600"
                    loading="lazy"
                    decoding="async"
                  />
                  <p v-else class="detection-art grid content-center rounded-lg border border-gray-200 p-3 text-center text-xs text-gray-600 dark:border-dark-700 dark:text-dark-300">
                    {{ t(group.sample.pelican_status === 'untested' ? 'channelDetection.imageUntested' : 'channelDetection.imageError') }}
                  </p>
                </div>
              </figure>
            </template>
            <p v-else class="text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.noSample') }}</p>
          </div>
          <footer v-if="administrator && group.group_id" class="space-y-2 border-t border-gray-200 bg-gray-50 px-3 py-2.5 dark:border-dark-700 dark:bg-dark-900/40 sm:px-4">
            <div class="flex flex-wrap items-center justify-between gap-x-3 gap-y-2">
              <button
                type="button"
                class="btn btn-secondary btn-sm gap-1.5"
                :disabled="pendingTriggers.has(group.group_id) || group.trigger_state === 'running' || group.trigger_state === 'busy'"
                @click="trigger(group.group_id)"
              >
                <Icon name="play" size="sm" aria-hidden="true" />
                {{ t(pendingTriggers.has(group.group_id) ? 'channelDetection.triggering' : group.trigger_state === 'running' ? 'channelDetection.running' : 'channelDetection.trigger') }}
              </button>
              <div v-if="group.confirmation && group.sample_id" class="ml-auto flex items-center gap-2 text-xs text-gray-700 dark:text-dark-200">
                <span>{{ t('channelDetection.degradationQuestion') }}</span>
                <button type="button" class="btn btn-secondary btn-sm" @click="openConfirmation(group)">{{ t('channelDetection.yes') }}</button>
              </div>
            </div>
            <p v-if="group.trigger_state === 'busy' || group.trigger_state === 'no_send'" role="status" class="text-xs text-gray-600 dark:text-dark-300">
              {{ t(group.trigger_state === 'busy' ? 'channelDetection.busy' : 'channelDetection.noSend') }}
            </p>
          </footer>
        </article>
      </div>
    </section>
    <ConfirmDialog
      :show="administrator && !!confirmation"
      :title="t('channelDetection.confirmTitle')"
      :message="t('channelDetection.confirmMessage', { account: confirmation?.scope.account_name ?? '' })"
      :confirm-text="t(confirming ? 'channelDetection.confirming' : 'channelDetection.confirmAction')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirm"
      @cancel="closeConfirmation"
    >
      <ul v-if="confirmation" class="list-disc space-y-1 pl-5 text-sm text-gray-700 dark:text-dark-200">
        <li v-for="group in confirmation.scope.groups" :key="group.id" class="[overflow-wrap:anywhere]">{{ group.name }}</li>
      </ul>
    </ConfirmDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import { channelDetectionAPI, type AdminDetectionGroup, type DetectionConfirmationScope, type PublicDetectionGroup } from '@/api/channelDetection'
import { useAppStore, useAuthStore } from '@/stores'
import { useChannelDetectionStore } from '@/stores/channelDetection'

const { t, locale } = useI18n()
const detectionStore = useChannelDetectionStore()
const authStore = useAuthStore()
const appStore = useAppStore()
const administrator = computed(() => authStore.isAuthenticated && authStore.isAdmin)
type DetectionCard = PublicDetectionGroup & Partial<AdminDetectionGroup>
const groups = ref<DetectionCard[]>([])
const loading = ref(true)
const errorKey = ref('')
const pendingTriggers = ref(new Set<number>())
const confirming = ref(false)
const confirmation = ref<{ groupID: number; sampleID: string; scope: DetectionConfirmationScope }>()
let request: AbortController | undefined
let refreshTimer: number | undefined
let runningTimer: number | undefined
let mounted = false

async function load(replace = false) {
  if (!mounted || (request && !replace)) return
  request?.abort()
  const controller = new AbortController()
  request = controller
  try {
    const results = administrator.value
      ? await channelDetectionAPI.adminResults(controller.signal)
      : await channelDetectionAPI.results(controller.signal)
    if (controller.signal.aborted) return
    groups.value = results
    errorKey.value = ''
    detectionStore.setPublicEnabled(true)
  } catch (error) {
    if (controller.signal.aborted) return
    const { status } = error as { status?: number }
    groups.value = []
    errorKey.value = status === 404 ? 'channelDetection.publicOff' : 'channelDetection.loadError'
    if (status === 404) detectionStore.setPublicEnabled(false)
  } finally {
    if (!controller.signal.aborted) loading.value = false
    if (request === controller) request = undefined
  }
}

async function trigger(groupID: number) {
  if (pendingTriggers.value.has(groupID) || !administrator.value) return
  pendingTriggers.value.add(groupID)
  try {
    await channelDetectionAPI.trigger(groupID)
  } catch (error) {
    const reason = (error as { reason?: string }).reason
    appStore.showError(t(reason === 'CHANNEL_DETECTION_BUSY' ? 'channelDetection.busyRetry' : 'channelDetection.triggerError'))
  } finally {
    await load(true)
    pendingTriggers.value.delete(groupID)
  }
}

function openConfirmation(group: DetectionCard) {
  if (administrator.value && !confirming.value && group.group_id && group.sample_id && group.confirmation) {
    confirmation.value = { groupID: group.group_id, sampleID: group.sample_id, scope: group.confirmation }
  }
}

function closeConfirmation() {
  if (!confirming.value) confirmation.value = undefined
}

async function confirm() {
  const target = confirmation.value
  if (!target || confirming.value || !administrator.value) return
  confirming.value = true
  try {
    const result = await channelDetectionAPI.confirm(target.groupID, target.sampleID, target.scope.scope_version)
    appStore.showSuccess(t(result.sync_pending ? 'channelDetection.confirmationPending' : 'channelDetection.confirmed', {
      until: formatTime(result.until),
    }))
  } catch (error) {
    const { status, reason } = error as { status?: number; reason?: string }
    const key = reason === 'CHANNEL_DETECTION_ALREADY_PROTECTED'
      ? 'channelDetection.alreadyProtected'
      : status === 409 || status === 404 ? 'channelDetection.confirmationChanged' : 'channelDetection.confirmationError'
    appStore.showError(t(key))
  } finally {
    confirmation.value = undefined
    await load(true)
    confirming.value = false
  }
}

function imageSource(svg: string) {
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

function formatTime(date: string) {
  return new Intl.DateTimeFormat(locale.value, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(date))
}

function loadIfVisible() {
  if (!document.hidden) void load()
}

onMounted(() => {
  mounted = true
  void load()
  refreshTimer = window.setInterval(loadIfVisible, 60_000)
  runningTimer = window.setInterval(() => {
    if (administrator.value && groups.value.some(group => group.trigger_state === 'running' || group.trigger_state === 'busy')) {
      loadIfVisible()
    }
  }, 4_000)
  document.addEventListener('visibilitychange', loadIfVisible)
})

onBeforeUnmount(() => {
  mounted = false
  request?.abort()
  window.clearInterval(refreshTimer)
  window.clearInterval(runningTimer)
  document.removeEventListener('visibilitychange', loadIfVisible)
})

watch(administrator, () => {
  confirmation.value = undefined
  void load(true)
})
</script>

<style scoped>
.detection-preview {
  container-type: size;
}

.detection-art {
  width: min(100%, 133.333cqh);
  aspect-ratio: 4 / 3;
}
</style>
