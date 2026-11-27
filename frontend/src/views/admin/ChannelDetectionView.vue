<template>
  <AppLayout>
    <h1 class="page-title lg:hidden">{{ t('channelDetection.adminTitle') }}</h1>
    <p v-if="loading" role="status" class="text-sm text-gray-500 dark:text-dark-400">{{ t('common.loading') }}</p>
    <div v-else-if="!config" class="card space-y-3 p-5">
      <p role="alert">{{ t(loadError) }}</p>
      <button type="button" class="btn btn-secondary" @click="load">{{ t('channelDetection.retry') }}</button>
    </div>
    <form v-else class="max-w-5xl space-y-5" @submit.prevent="save">
      <section class="card space-y-4 p-5" aria-labelledby="detection-schedule">
        <div class="flex items-start justify-between gap-4">
          <div>
            <h2 id="detection-schedule" class="font-serif text-lg text-gray-950 dark:text-dark-50">{{ t('channelDetection.schedule') }}</h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('channelDetection.scheduleHint') }}</p>
          </div>
          <Toggle v-model="config.enabled" :disabled="saving" aria-labelledby="detection-schedule" />
        </div>
        <div class="max-w-xs">
          <label for="detection-interval" class="input-label">{{ t('channelDetection.interval') }}</label>
          <input id="detection-interval" v-model.number="config.interval_minutes" class="input" type="number" min="15" max="1440" step="1" required :disabled="saving" />
        </div>
      </section>

      <section class="card" aria-labelledby="detection-groups">
        <header class="space-y-1 px-4 py-4 sm:px-5">
          <h2 id="detection-groups" class="font-serif text-lg text-gray-950 dark:text-dark-50">{{ t('channelDetection.groups') }}</h2>
          <p class="text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.groupModelsHint') }}</p>
          <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('channelDetection.groupLimit') }}</p>
        </header>
        <p v-if="directoryLoading" role="status" class="px-4 pb-4 text-sm text-gray-600 dark:text-dark-300 sm:px-5">{{ t('common.loading') }}</p>
        <div v-else-if="directoryError" class="flex flex-wrap items-center gap-2 px-4 pb-4 sm:px-5">
          <p role="alert" class="text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.directoryError') }}</p>
          <button type="button" class="btn btn-secondary" @click="loadGroups">{{ t('channelDetection.retry') }}</button>
        </div>
        <p v-else-if="!rows.length" class="px-4 pb-4 text-sm text-gray-600 dark:text-dark-300 sm:px-5">{{ t('channelDetection.emptyDirectory') }}</p>
        <ul v-if="rows.length" class="divide-y divide-gray-200 border-t border-gray-200 dark:divide-dark-700 dark:border-dark-700">
          <li v-for="group in rows" :key="group.id" class="space-y-3 px-4 py-4 sm:px-5">
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <h3 :id="'detection-group-' + group.id" class="break-words text-sm font-medium text-gray-950 dark:text-dark-50">{{ group.name }}</h3>
                <p v-if="group.message" :id="'detection-group-status-' + group.id" class="mt-1 text-sm text-gray-500 dark:text-dark-400">
                  {{ t(group.message) }}
                </p>
              </div>
              <Toggle
                :model-value="Boolean(group.selection)"
                :disabled="saving || (!group.selection && (!group.selectable || config.groups.length >= 16))"
                :aria-labelledby="'detection-group-' + group.id"
                :aria-describedby="group.message ? 'detection-group-status-' + group.id : undefined"
                class="disabled:cursor-not-allowed disabled:opacity-50"
                @update:model-value="setGroup(group.id, $event)"
              />
            </div>
            <div v-if="group.selection" class="max-w-md">
              <label :for="'detection-model-' + group.id" class="input-label">{{ t('channelDetection.model') }}</label>
              <Select
                :id="'detection-model-' + group.id"
                :aria-label="t('channelDetection.modelForGroup', { group: group.name })"
                :model-value="group.selection.model || null"
                :options="modelOptions(group.selection)"
                :placeholder="t('channelDetection.selectModel')"
                :disabled="saving || !group.selectable || modelLoading.has(group.id) || !models[group.id]?.length"
                searchable
                @update:model-value="setModel(group.selection, $event)"
              />
              <p v-if="modelLoading.has(group.id)" role="status" class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ t('common.loading') }}</p>
              <div v-else-if="modelErrors.has(group.id) && group.selectable" class="mt-2 flex flex-wrap items-center gap-2">
                <p role="alert" class="text-sm text-gray-600 dark:text-dark-300">{{ t('channelDetection.modelLoadError') }}</p>
                <button type="button" class="btn btn-secondary" @click="loadModels(group.id)">{{ t('channelDetection.retry') }}</button>
              </div>
              <p v-else-if="group.selectable && models[group.id]?.length === 0" class="mt-2 text-sm text-gray-500 dark:text-dark-400">{{ t('channelDetection.noModels') }}</p>
            </div>
          </li>
        </ul>
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
import { channelDetectionAPI, type DetectionConfig, type DetectionGroupOption, type DetectionSelection } from '@/api/channelDetection'
import { useChannelDetectionStore } from '@/stores/channelDetection'

const { t } = useI18n()
const detectionStore = useChannelDetectionStore()
const config = ref<DetectionConfig>()
const groups = ref<DetectionGroupOption[]>([])
const models = reactive<Record<number, string[]>>({})
const modelLoading = reactive(new Set<number>())
const modelErrors = reactive(new Set<number>())
const loading = ref(true)
const loadError = ref('channelDetection.loadError')
const directoryError = ref(false)
const directoryLoading = ref(false)
const saving = ref(false)
const saved = ref(false)
const saveError = ref('')

const rows = computed(() => {
  const selected = new Map(config.value?.groups.map(group => [group.group_id, group]))
  const known = new Set(groups.value.map(group => group.id))
  const result = groups.value.map(group => ({
    id: group.id,
    name: group.name,
    selection: selected.get(group.id),
    selectable: group.active && group.has_models && !group.mode_restricted && !group.claude_code_only,
    message: !group.active ? 'channelDetection.groupDisabled'
      : group.claude_code_only ? 'channelDetection.groupClaudeCodeOnly'
      : group.mode_restricted ? 'channelDetection.groupModeRestricted'
      : !group.has_models ? 'channelDetection.noModels' : '',
  }))
  for (const [id, selection] of selected) {
    if (known.has(id)) continue
    result.push({
      id,
      name: t('channelDetection.unavailableGroup', { id }),
      selection,
      selectable: false,
      message: directoryLoading.value || directoryError.value ? '' : 'channelDetection.groupDeleted',
    })
  }
  return result
})

function modelOptions(selection: DetectionSelection) {
  const available = models[selection.group_id] ?? []
  const options = available.map(model => ({ value: model, label: model, disabled: false }))
  if (selection.model && !available.includes(selection.model)) {
    const label = models[selection.group_id] ? t('channelDetection.staleModel', { model: selection.model }) : selection.model
    options.unshift({ value: selection.model, label, disabled: true })
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
  } catch (error) {
    const { status } = error as { status?: number }
    loadError.value = status === 404 ? 'channelDetection.featureOff' : 'channelDetection.loadError'
  } finally {
    loading.value = false
  }
}

async function loadGroups() {
  directoryLoading.value = true
  directoryError.value = false
  try {
    groups.value = await channelDetectionAPI.groups()
    for (const row of rows.value) {
      if (row.selection && row.selectable) void loadModels(row.id)
    }
  } catch {
    groups.value = []
    directoryError.value = true
  } finally {
    directoryLoading.value = false
  }
}

function setGroup(id: number, enabled: boolean) {
  if (!config.value || saving.value) return
  if (!enabled) {
    config.value.groups = config.value.groups.filter(group => group.group_id !== id)
    if (config.value.groups.length === 0) config.value.enabled = false
    return
  }
  const row = rows.value.find(group => group.id === id)
  if (!row?.selectable || row.selection || config.value.groups.length >= 16) return
  config.value.groups.push({ group_id: id, model: '' })
  void loadModels(id)
}

function setModel(selection: DetectionSelection | undefined, model: unknown) {
  if (selection && typeof model === 'string' && models[selection.group_id]?.includes(model)) {
    selection.model = model
  }
}

const saveErrors: Record<string, string> = {
  CHANNEL_DETECTION_DISABLED: 'channelDetection.featureOff',
  CHANNEL_DETECTION_INVALID_CONFIG: 'channelDetection.invalidConfig',
  CHANNEL_DETECTION_INVALID_MODEL: 'channelDetection.invalidModel',
}

async function save() {
  if (!config.value || saving.value) return
  saved.value = false
  saveError.value = ''
  if (config.value.groups.some(group => !group.model)) {
    saveError.value = 'channelDetection.invalidModel'
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
