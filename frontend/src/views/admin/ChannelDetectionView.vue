<template>
  <AppLayout>
    <p v-if="loading" role="status" class="text-sm text-gray-500 dark:text-dark-400">{{ t('common.loading') }}</p>
    <div v-else-if="!config" class="card space-y-3 p-5">
      <p role="alert">{{ t(loadError) }}</p>
      <button type="button" class="btn btn-secondary" @click="load">{{ t('channelDetection.retry') }}</button>
    </div>
    <form v-else class="max-w-5xl space-y-5" @submit.prevent="save">
      <h1 class="sr-only">{{ t('channelDetection.adminTitle') }}</h1>
      <section class="card space-y-4 p-5" aria-labelledby="detection-schedule">
        <div class="flex items-start justify-between gap-4">
          <div>
            <h2 id="detection-schedule" class="font-semibold text-gray-900 dark:text-white">{{ t('channelDetection.schedule') }}</h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('channelDetection.scheduleHint') }}</p>
          </div>
          <Toggle v-model="config.enabled" :disabled="saving" aria-labelledby="detection-schedule" />
        </div>
        <div class="max-w-xs">
          <label for="detection-interval" class="input-label">{{ t('channelDetection.interval') }}</label>
          <input id="detection-interval" v-model.number="config.interval_minutes" class="input" type="number" min="15" max="1440" step="1" required :disabled="saving" />
        </div>
      </section>

      <section class="card space-y-4 p-5" aria-labelledby="detection-groups">
        <div>
          <h2 id="detection-groups" class="font-semibold text-gray-900 dark:text-white">{{ t('channelDetection.groups') }}</h2>
          <p id="detection-key-hint" class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('channelDetection.keyHint') }}</p>
        </div>
        <article v-for="selection in config.groups" :key="selection.group_id" class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-700">
          <div class="flex items-center justify-between gap-3">
            <h3 class="min-w-0 break-words font-medium text-gray-900 dark:text-white">{{ groupName(selection.group_id) }}</h3>
            <button type="button" class="btn btn-secondary shrink-0" :disabled="saving" @click="removeGroup(selection.group_id)">
              {{ t('channelDetection.remove') }}
            </button>
          </div>
          <div class="grid gap-3 sm:grid-cols-2">
            <div class="min-w-0">
              <label :for="`detection-model-${selection.group_id}`" class="input-label">{{ t('channelDetection.model') }}</label>
              <Select
                :id="`detection-model-${selection.group_id}`"
                :aria-label="t('channelDetection.model')"
                :model-value="selection.model"
                :options="modelOptions(selection)"
                :placeholder="t('channelDetection.selectModel')"
                :disabled="saving || modelLoading.has(selection.group_id)"
                searchable
                @update:model-value="selection.model = String($event ?? '')"
              />
              <p v-if="modelLoading.has(selection.group_id)" role="status" class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('common.loading') }}</p>
              <p v-else-if="modelErrors.has(selection.group_id)" role="status" class="mt-1 text-sm text-gray-500 dark:text-dark-400">
                {{ t('channelDetection.modelLoadError') }}
                <button type="button" class="font-medium underline" @click="loadModels(selection.group_id)">{{ t('channelDetection.retry') }}</button>
              </p>
              <p v-else-if="!models[selection.group_id]?.length" class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('channelDetection.noModels') }}</p>
            </div>
            <div class="min-w-0">
              <label :for="`detection-key-${selection.group_id}`" class="input-label">{{ t('channelDetection.keyID') }}</label>
              <input
                :id="`detection-key-${selection.group_id}`"
                :value="selection.api_key_id || ''"
                type="number"
                min="1"
                max="9007199254740991"
                step="1"
                inputmode="numeric"
                class="input"
                aria-describedby="detection-key-hint"
                :disabled="saving"
                @input="selection.api_key_id = Number(($event.target as HTMLInputElement).value)"
              />
            </div>
          </div>
        </article>
        <div class="max-w-sm">
          <label for="detection-add-group" class="input-label">{{ t('channelDetection.addGroup') }}</label>
          <Select
            id="detection-add-group"
            :aria-label="t('channelDetection.addGroup')"
            :model-value="null"
            :options="availableGroups"
            :placeholder="t('channelDetection.selectGroup')"
            :disabled="saving || config.groups.length >= 16 || !availableGroups.length"
            searchable
            @update:model-value="addGroup"
          />
          <p v-if="directoryError" role="status" class="mt-1 text-sm text-gray-500 dark:text-dark-400">
            {{ t('channelDetection.directoryError') }}
            <button type="button" class="font-medium underline" @click="loadGroups">{{ t('channelDetection.retry') }}</button>
          </p>
          <p v-else class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('channelDetection.groupLimit') }}</p>
        </div>
      </section>

      <div class="flex flex-wrap items-center gap-3">
        <button type="submit" class="btn btn-primary" :disabled="saving">{{ t(saving ? 'common.loading' : 'common.save') }}</button>
        <p v-if="saveError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ t(saveError) }}</p>
        <p v-if="saved" role="status" class="text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.saved') }}</p>
      </div>
    </form>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Select from '@/components/common/Select.vue'
import Toggle from '@/components/common/Toggle.vue'
import { channelDetectionAPI, type DetectionConfig, type DetectionSelection } from '@/api/channelDetection'
import { useChannelDetectionStore } from '@/stores/channelDetection'

const { t } = useI18n()
const detectionStore = useChannelDetectionStore()
const config = ref<DetectionConfig>()
const groups = ref<{ id: number; name: string }[]>([])
const models = reactive<Record<number, string[]>>({})
const modelLoading = reactive(new Set<number>())
const modelErrors = reactive(new Set<number>())
const loading = ref(true)
const loadError = ref('channelDetection.loadError')
const directoryError = ref(false)
const saving = ref(false)
const saved = ref(false)
const saveError = ref('')

const availableGroups = computed(() => {
  const selected = new Set(config.value?.groups.map(group => group.group_id))
  return groups.value.filter(group => !selected.has(group.id)).map(group => ({ value: group.id, label: group.name }))
})

function groupName(id: number) {
  return groups.value.find(group => group.id === id)?.name ?? t('channelDetection.unavailableGroup', { id })
}

function modelOptions(selection: DetectionSelection) {
  const available = models[selection.group_id] ?? []
  const options = available.map(model => ({ value: model, label: model }))
  if (selection.model && !available.includes(selection.model)) {
    const label = models[selection.group_id] ? t('channelDetection.staleModel', { model: selection.model }) : selection.model
    options.unshift({ value: selection.model, label })
  }
  return options
}

async function loadModels(id: number) {
  if (modelLoading.has(id)) return
  modelLoading.add(id)
  modelErrors.delete(id)
  try {
    models[id] = await channelDetectionAPI.models(id)
  } catch {
    modelErrors.add(id)
  } finally {
    modelLoading.delete(id)
  }
}

async function load() {
  loading.value = true
  try {
    config.value = await channelDetectionAPI.config()
    // Keep a usable pausing control even if the group directory is unavailable.
    void loadGroups()
    for (const group of config.value.groups) void loadModels(group.group_id)
  } catch (error) {
    const { status } = error as { status?: number }
    loadError.value = status === 404 ? 'channelDetection.featureOff' : 'channelDetection.loadError'
  } finally {
    loading.value = false
  }
}

async function loadGroups() {
  directoryError.value = false
  try {
    groups.value = await channelDetectionAPI.groups()
  } catch {
    groups.value = []
    directoryError.value = true
  }
}

function addGroup(id: string | number | boolean | null) {
  if (typeof id !== 'number' || !config.value || config.value.groups.length >= 16) return
  config.value.groups.push({ group_id: id, model: '', api_key_id: 0 })
  void loadModels(id)
}

function removeGroup(id: number) {
  if (config.value) config.value.groups = config.value.groups.filter(group => group.group_id !== id)
}

const saveErrors: Record<string, string> = {
  CHANNEL_DETECTION_DISABLED: 'channelDetection.featureOff',
  CHANNEL_DETECTION_INVALID_CONFIG: 'channelDetection.invalidConfig',
  CHANNEL_DETECTION_INVALID_MODEL: 'channelDetection.invalidModel',
  CHANNEL_DETECTION_INVALID_KEY: 'channelDetection.invalidKey',
}

async function save() {
  if (!config.value || saving.value) return
  saved.value = false
  saveError.value = ''
  if (config.value.groups.some(group => !Number.isSafeInteger(group.api_key_id))) {
    saveError.value = 'channelDetection.invalidKey'
    return
  }
  saving.value = true
  try {
    await channelDetectionAPI.save(config.value)
    saved.value = true
    if (!config.value.enabled) detectionStore.setPublicEnabled(false)
    void detectionStore.refresh(true)
  } catch (error) {
    const { reason } = error as { reason?: string }
    saveError.value = (reason && saveErrors[reason]) || 'channelDetection.saveError'
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>
