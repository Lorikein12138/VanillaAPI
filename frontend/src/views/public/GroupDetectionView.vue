<template>
  <AppLayout>
    <section class="mx-auto max-w-5xl space-y-4">
      <h1 class="page-title lg:hidden">{{ t('channelDetection.publicTitle') }}</h1>
      <p v-if="loading" role="status" class="card p-5 text-sm text-gray-600 dark:text-dark-300">{{ t('common.loading') }}</p>
      <div v-else-if="errorKey" class="card space-y-3 p-5">
        <p role="status">{{ t(errorKey) }}</p>
        <button v-if="errorKey === 'channelDetection.loadError'" type="button" class="btn btn-secondary" @click="load">{{ t('channelDetection.retry') }}</button>
      </div>
      <p v-else-if="!groups.length" class="card p-5 text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.empty') }}</p>
      <div v-else class="grid grid-cols-[repeat(auto-fill,minmax(min(100%,19rem),1fr))] gap-4">
        <article v-for="group in groups" :key="group.name" class="card grid aspect-square min-w-0 grid-rows-[auto_minmax(0,1fr)_minmax(0,36%)] gap-2 p-3 sm:p-4">
          <header class="min-w-0 border-b border-gray-200 pb-2 dark:border-dark-700">
            <h2 :title="group.name" class="truncate font-serif text-xl font-normal leading-6 text-gray-950 dark:text-dark-50">{{ group.name }}</h2>
            <div class="grid grid-rows-[1.25rem_1rem]">
              <p :title="group.model" class="truncate text-sm leading-5 text-gray-600 dark:text-dark-300">{{ group.model }}</p>
              <time v-if="group.sample" :datetime="group.sample.checked_at" class="text-xs leading-4 text-gray-500 dark:text-dark-400">
                {{ formatTime(group.sample.checked_at) }}
              </time>
            </div>
          </header>
          <template v-if="group.sample">
            <dl class="grid min-h-0 grid-cols-2 content-start gap-3 overflow-auto">
              <div v-for="metric in metrics(group.sample)" :key="metric.key" class="min-w-0 [overflow-wrap:anywhere]">
                <dt class="text-sm font-medium leading-5 text-gray-700 dark:text-dark-200">{{ t('channelDetection.' + metric.key) }}</dt>
                <dd class="font-serif text-lg leading-6 tabular-nums text-gray-950 dark:text-dark-50">{{ t('channelDetection.' + metric.key + 'Count', { count: metric.counts.passed }) }}</dd>
                <dd v-for="counter in descriptions(metric.counts, metric.key)" :key="counter" class="text-xs leading-4 text-gray-600 dark:text-dark-300">{{ counter }}</dd>
              </div>
            </dl>
            <figure class="grid min-w-0 grid-rows-[auto_minmax(0,1fr)] gap-1">
              <figcaption class="text-sm font-medium text-gray-700 dark:text-dark-200">{{ t('channelDetection.pelican') }}</figcaption>
              <div class="grid min-h-0 min-w-0 grid-cols-1 grid-rows-1 place-items-center overflow-auto rounded-lg border border-gray-200 bg-gray-50 dark:border-dark-700 dark:bg-dark-900">
                <img
                  v-if="group.sample.pelican_status === 'generated' && group.sample.svg"
                  :src="imageSource(group.sample.svg)"
                  :alt="t('channelDetection.pelicanAlt')"
                  class="h-full min-h-0 w-full min-w-0 object-contain"
                  loading="lazy"
                  decoding="async"
                />
                <p v-else class="p-2 text-center text-xs text-gray-600 dark:text-dark-300">{{ t(group.sample.pelican_status === 'untested' ? 'channelDetection.imageUntested' : 'channelDetection.imageError') }}</p>
              </div>
            </figure>
          </template>
          <p v-else class="text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.noSample') }}</p>
        </article>
      </div>
    </section>
  </AppLayout>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { channelDetectionAPI, type DetectionCounts, type DetectionSample, type PublicDetectionGroup } from '@/api/channelDetection'
import { useChannelDetectionStore } from '@/stores/channelDetection'

const { t, locale } = useI18n()
const detectionStore = useChannelDetectionStore()
const groups = ref<PublicDetectionGroup[]>([])
const loading = ref(true)
const errorKey = ref('')
let request: AbortController | undefined
let refreshTimer: number | undefined

async function load() {
  if (request) return
  const controller = new AbortController()
  request = controller
  try {
    groups.value = await channelDetectionAPI.results(controller.signal)
    if (controller.signal.aborted) return
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
    request = undefined
  }
}

function imageSource(svg: string) {
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

function formatTime(date: string) {
  return new Intl.DateTimeFormat(locale.value, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(date))
}

function metrics(sample: DetectionSample) {
  return [
    { key: 'reasoning' as const, counts: sample.reasoning },
    { key: 'fingerprint' as const, counts: sample.fingerprint },
  ]
}

function descriptions(counts: DetectionCounts, metric: 'reasoning' | 'fingerprint') {
  const details: string[] = []
  if (counts.failed) details.push(t(metric === 'reasoning' ? 'channelDetection.incorrect' : 'channelDetection.mismatched', { count: counts.failed }))
  if (counts.errors) details.push(t('channelDetection.errors', { count: counts.errors }))
  if (counts.unattributed) details.push(t('channelDetection.unattributed', { count: counts.unattributed }))
  if (counts.untested) details.push(t('channelDetection.untested', { count: counts.untested }))
  return details
}

function loadIfVisible() {
  if (!document.hidden) void load()
}

onMounted(() => {
  void load()
  refreshTimer = window.setInterval(loadIfVisible, 60_000)
  document.addEventListener('visibilitychange', loadIfVisible)
})

onBeforeUnmount(() => {
  request?.abort()
  window.clearInterval(refreshTimer)
  document.removeEventListener('visibilitychange', loadIfVisible)
})
</script>
