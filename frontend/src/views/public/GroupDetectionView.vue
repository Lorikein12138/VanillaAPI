<template>
  <AppLayout>
    <section class="mx-auto max-w-5xl space-y-4">
      <h1 class="page-title lg:hidden">{{ t('channelDetection.publicTitle') }}</h1>
      <p class="max-w-prose text-sm leading-relaxed text-gray-600 dark:text-dark-300">{{ t('channelDetection.publicDescription') }}</p>
      <p v-if="loading" role="status" class="card p-5 text-sm text-gray-600 dark:text-dark-300">{{ t('common.loading') }}</p>
      <div v-else-if="errorKey" class="card space-y-3 p-5">
        <p role="status">{{ t(errorKey) }}</p>
        <button v-if="errorKey === 'channelDetection.loadError'" type="button" class="btn btn-secondary" @click="load">{{ t('channelDetection.retry') }}</button>
      </div>
      <p v-else-if="!groups.length" class="card p-5 text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.empty') }}</p>
      <article v-for="group in groups" :key="group.name" class="card p-4 sm:p-5">
        <header class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
          <div class="min-w-0">
            <h2 class="break-words font-serif text-xl text-gray-950 dark:text-dark-50">{{ group.name }}</h2>
            <p v-if="group.model" class="break-words [overflow-wrap:anywhere] text-sm text-gray-600 dark:text-dark-300">{{ group.model }}</p>
          </div>
          <time v-if="group.sample" :datetime="group.sample.checked_at" class="text-sm text-gray-500 dark:text-dark-400">
            {{ formatTime(group.sample.checked_at) }}
          </time>
        </header>
        <div v-if="group.sample" class="mt-4 grid gap-5 border-t border-gray-200 pt-4 dark:border-dark-700 md:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
          <dl class="grid content-start grid-cols-2 gap-3 md:grid-cols-1 xl:grid-cols-2">
            <div v-for="metric in metrics(group.sample)" :key="metric.key" class="min-w-0 space-y-1">
              <dt class="text-sm font-medium text-gray-700 dark:text-dark-200">{{ t('channelDetection.' + metric.key) }}</dt>
              <dd class="font-serif text-xl tabular-nums text-gray-950 dark:text-dark-50">{{ t('channelDetection.' + metric.key + 'Count', { count: metric.counts.passed }) }}</dd>
              <dd v-for="counter in descriptions(metric.counts, metric.key)" :key="counter" class="text-sm text-gray-600 dark:text-dark-300">{{ counter }}</dd>
            </div>
          </dl>
          <figure class="min-w-0 space-y-2">
            <figcaption class="text-sm font-medium text-gray-700 dark:text-dark-200">{{ t('channelDetection.pelican') }}</figcaption>
            <img
              v-if="group.sample.pelican_status === 'generated' && group.sample.svg"
              :src="imageSource(group.sample.svg)"
              :alt="t('channelDetection.pelicanAlt')"
              class="aspect-[4/3] w-full max-w-sm rounded-lg border border-gray-200 bg-gray-50 object-contain dark:border-dark-700 dark:bg-dark-900"
              loading="lazy"
              decoding="async"
            />
            <p v-else class="text-sm text-gray-600 dark:text-dark-300">{{ t(group.sample.pelican_status === 'untested' ? 'channelDetection.imageUntested' : 'channelDetection.imageError') }}</p>
          </figure>
        </div>
        <p v-else class="mt-4 border-t border-gray-200 pt-4 text-sm text-gray-600 dark:border-dark-700 dark:text-dark-300">{{ t('channelDetection.noSample') }}</p>
      </article>
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
