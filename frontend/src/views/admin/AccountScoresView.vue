<template>
  <AppLayout>
    <div v-if="!enabled" class="card space-y-3 p-5">
      <p v-if="checkingSettings" role="status">{{ t('common.loading') }}</p>
      <template v-else>
        <p role="status">{{ label(adminSettings.gatewayAdaptiveSchedulingEnabled === false ? 'disabled' : 'settingsLoadFailed') }}</p>
        <button type="button" class="btn btn-secondary" @click="checkSettings">{{ t('common.refresh') }}</button>
      </template>
    </div>
    <div v-else class="space-y-4">
      <div :class="{ 'card px-4 pb-6': selectedGroupId === null }">
        <EmptyState
          v-if="selectedGroupId === null"
          class="pb-3 pt-6"
          :title="label('chooseGroup')"
          :description="label('chooseGroupHint')"
        />
        <div
          class="flex flex-wrap items-center gap-3"
          :class="selectedGroupId === null ? 'justify-center' : 'justify-between'"
        >
          <div class="w-full min-w-0 sm:w-64">
            <Select
              id="account-scores-group"
              :model-value="selectedGroupId"
              :aria-label="label('chooseGroup')"
              :options="groupOptions"
              :placeholder="label('chooseGroup')"
              :search-placeholder="t('admin.groups.searchGroups')"
              :loading="groupsLoading"
              searchable
              clearable
              @update:model-value="selectGroup"
            />
          </div>
          <div v-if="selectedGroupId !== null" class="flex flex-wrap gap-2">
            <button type="button" class="btn btn-secondary" :disabled="scoresLoading" @click="loadScores">
              <Icon name="refresh" size="md" class="mr-2" />{{ t('common.refresh') }}
            </button>
            <button type="button" class="btn btn-secondary" @click="autoRefresh.setEnabled(!autoEnabled)">
              {{ label(autoEnabled ? 'pause' : 'resume') }}
            </button>
          </div>
        </div>
        <div v-if="missingGroup || groupsError" class="mt-2 text-center text-sm text-red-600 dark:text-red-400">
          <p v-if="missingGroup" role="status">{{ label('missingGroup') }}</p>
          <p v-if="groupsError" role="status">{{ label('groupLoadFailed') }}</p>
          <button type="button" class="btn btn-ghost btn-sm" :disabled="groupsLoading" @click="loadGroups">{{ t('common.refresh') }}</button>
        </div>
      </div>

      <template v-if="selectedGroupId !== null">
        <div v-if="snapshot || scoresError" class="space-y-1">
          <p v-if="snapshot" class="text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.accountScores.latestSnapshot', { time: dateFormatter.format(new Date(snapshot.observed_at)) }) }}
          </p>
          <div v-if="snapshot?.items.length" class="flex items-center text-xs text-gray-500 dark:text-gray-400 md:hidden">
            <span>{{ label('overallScore') }}</span>
            <HelpTooltip :content="label('overallHint')" trigger="click" width-class="w-48" />
          </div>
          <p v-if="scoresError" class="text-sm text-red-600 dark:text-red-400" role="status">{{ label(scoresError) }}</p>
        </div>
        <DataTable
          v-if="snapshot || scoresLoading"
          class="md:card"
          :columns="columns"
          :data="snapshot?.items ?? []"
          :loading="scoresLoading && !snapshot"
          :expandable-actions="false"
        >
          <template #empty>
            <p class="p-6 text-center text-sm text-gray-500 dark:text-gray-400">{{ label('noMembers') }}</p>
          </template>
          <template #cell-name="{ row }">
            <div class="min-w-0 whitespace-normal font-medium text-gray-900 [overflow-wrap:anywhere] dark:text-white">{{ row.name }}</div>
            <span class="text-xs text-gray-500 dark:text-gray-400">#{{ row.id }} / {{ row.platform }} / {{ row.type }}</span>
          </template>
          <template #header-overall>
            <span>{{ label('overallScore') }}</span>
            <HelpTooltip :content="label('overallHint')" width-class="w-80" />
          </template>
          <template #cell-overall="{ row }">
            <span class="font-semibold tabular-nums">{{ row.overall_score ?? '—' }}</span>
            <span class="block text-xs text-gray-500 dark:text-gray-400">
              {{ t(row.overall_score === null && row.overall_sample_count > 0
                ? 'admin.accountScores.overallInsufficient'
                : 'admin.accountScores.overallSamples', { count: row.overall_sample_count }) }}
            </span>
          </template>
          <template #cell-status="{ row }">
            <span :class="['badge text-xs', row.schedulable_now ? 'badge-success' : 'badge-warning']">
              {{ schedulingStatus(row) }}
            </span>
          </template>
        </DataTable>
        <p v-if="snapshot && snapshot.total > memberTotal" class="mb-2 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.accountScores.memberPageLimit', { accessible: memberTotal, total: snapshot.total }) }}
        </p>
        <Pagination
          v-if="memberTotal > 0"
          :page="memberPage"
          :page-size="memberPageSize"
          :total="memberTotal"
          :show-page-size-selector="false"
          @update:page="changeMemberPage"
        />
      </template>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import type { AdaptiveAccountScoresPage, AdaptiveScoreMember } from '@/api/admin/groups'
import EmptyState from '@/components/common/EmptyState.vue'
import DataTable from '@/components/common/DataTable.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import type { Column } from '@/components/common/types'
import Icon from '@/components/icons/Icon.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { useAdminSettingsStore } from '@/stores/adminSettings'

const { t, locale } = useI18n()
const label = (key: string) => t(`admin.accountScores.${key}`)
const adminSettings = useAdminSettingsStore()
const settingsChecked = ref(false)
const checkingSettings = ref(true)
const enabled = computed(() => settingsChecked.value && adminSettings.gatewayAdaptiveSchedulingEnabled === true)
const groupOptions = ref<SelectOption[]>([])
const selectedGroupId = ref<number | null>(null)
const groupsLoading = ref(false)
const groupsError = ref(false)
const missingGroup = ref(false)
let groupsRequest: AbortController | null = null

const snapshot = shallowRef<AdaptiveAccountScoresPage | null>(null)
const memberPage = ref(1)
const memberPageSize = 20
const memberTotal = ref(0)
const scoresLoading = ref(false)
const scoresError = ref<'refreshFailed' | null>(null)
let scoresRequest: AbortController | null = null

const columns = computed<Column[]>(() => [
  { key: 'name', label: label('member') },
  { key: 'overall', label: label('overallScore'), class: 'text-right' },
  { key: 'status', label: label('schedulability') },
])

const autoRefresh = useAutoRefresh({
  storageKey: 'admin-account-scores-refresh',
  intervals: [15],
  defaultInterval: 15,
  defaultEnabled: true,
  onRefresh: loadScores,
  shouldPause: () => document.hidden || !enabled.value || selectedGroupId.value === null || scoresLoading.value,
})
const autoEnabled = autoRefresh.enabled

function restartCountdown(seconds: number) {
  autoRefresh.intervalSeconds.value = seconds
  autoRefresh.resetCountdown()
}

function disableIfUnavailable(error: unknown) {
  if ((error as { reason?: string } | null)?.reason !== 'ADAPTIVE_ACCOUNT_SCORES_DISABLED') return false
  adminSettings.setGatewayAdaptiveSchedulingEnabled(false)
  return true
}

async function loadGroups() {
  if (!enabled.value) return
  groupsRequest?.abort()
  const request = new AbortController()
  groupsRequest = request
  groupsLoading.value = true
  groupsError.value = false
  const isCurrent = () => groupsRequest === request && enabled.value
  try {
    const options: SelectOption[] = []
    let afterId = 0
    while (true) {
      const result = await adminAPI.groups.listAdaptiveScoreOptions(afterId, request.signal)
      if (!isCurrent()) return
      if (!Array.isArray(result?.items)) throw new Error('Invalid adaptive score options')
      let lastId = afterId
      for (const { id, name, description } of result.items) {
        if (!Number.isSafeInteger(id) || id <= lastId || typeof name !== 'string' || typeof description !== 'string') {
          throw new Error('Invalid adaptive score option')
        }
        options.push({ value: id, label: name, description })
        lastId = id
      }
      if (result.next_after_id === null) break
      if (lastId === afterId || result.next_after_id !== lastId) {
        throw new Error('Invalid adaptive score options cursor')
      }
      afterId = lastId
    }
    groupOptions.value = options
  } catch (error) {
    if (isCurrent() && !disableIfUnavailable(error)) groupsError.value = true
  } finally {
    if (groupsRequest === request) {
      groupsRequest = null
      groupsLoading.value = false
    }
  }
}

function selectGroup(value: SelectOption['value']) {
  const groupId = typeof value === 'number' ? value : null
  if (selectedGroupId.value === groupId) return
  missingGroup.value = false
  selectedGroupId.value = groupId
  memberPage.value = 1
  memberTotal.value = 0
}

function clearScores() {
  scoresRequest?.abort()
  scoresRequest = null
  scoresLoading.value = false
  scoresError.value = null
  snapshot.value = null
  restartCountdown(15)
}

function changeMemberPage(page: number) {
  if (page >= 1 && page <= Math.ceil(memberTotal.value / memberPageSize)) memberPage.value = page
}

async function loadScores() {
  const groupId = selectedGroupId.value
  if (!enabled.value || typeof groupId !== 'number' || scoresRequest) return
  const request = new AbortController()
  scoresRequest = request
  scoresLoading.value = true
  const page = memberPage.value
  const isCurrent = () => scoresRequest === request && enabled.value && selectedGroupId.value === groupId
    && memberPage.value === page
  try {
    const result = await adminAPI.groups.getAdaptiveAccountScores(groupId, page, memberPageSize, request.signal)
    if (!isCurrent()) return
    snapshot.value = result
    memberTotal.value = result.max_queryable_total
    scoresError.value = null
    restartCountdown(15)
  } catch (error) {
    if (!isCurrent()) return
    if (disableIfUnavailable(error)) return
    const failure = error as { status?: number } | null
    if (failure?.status === 404) {
      selectGroup(null)
      missingGroup.value = true
      groupOptions.value = groupOptions.value.filter((option) => option.value !== groupId)
      void loadGroups()
      return
    }
    scoresError.value = 'refreshFailed'
    restartCountdown(Math.min(60, autoRefresh.intervalSeconds.value * 2))
  } finally {
    if (scoresRequest === request) {
      scoresRequest = null
      scoresLoading.value = false
    }
  }
}

watch([selectedGroupId, memberPage], () => {
  clearScores()
  void loadScores()
})

watch(enabled, (on) => {
  if (on) {
    void loadGroups()
    autoRefresh.start()
    return
  }
  autoRefresh.stop()
  clearScores()
  groupsRequest?.abort()
  groupsRequest = null
  groupsLoading.value = false
  groupOptions.value = []
  groupsError.value = false
  missingGroup.value = false
  selectGroup(null)
}, { immediate: true })

async function checkSettings() {
  checkingSettings.value = true
  try {
    await adminSettings.refreshGatewayAdaptiveScheduling()
  } finally {
    settingsChecked.value = true
    checkingSettings.value = false
  }
}

onMounted(checkSettings)
onBeforeUnmount(() => {
  scoresRequest?.abort()
  scoresRequest = null
  groupsRequest?.abort()
  groupsRequest = null
})

const dateFormatter = computed(() => new Intl.DateTimeFormat(locale.value, { dateStyle: 'short', timeStyle: 'medium' }))

function schedulingStatus(row: AdaptiveScoreMember) {
  const at = snapshot.value ? Date.parse(snapshot.value.observed_at) : Date.now()
  const activeUntil = (value: string | null) => value !== null && Date.parse(value) > at
  if (row.schedulable_now) return label('schedulable')
  if (row.status !== 'active') return row.status === 'error' ? t('admin.accounts.status.error') : t('admin.accounts.status.inactive')
  if (activeUntil(row.rate_limit_reset_at)) return t('admin.accounts.status.rateLimited')
  if (activeUntil(row.overload_until)) return t('admin.accounts.status.overloaded')
  if (activeUntil(row.temp_unschedulable_until)) return t('admin.accounts.status.tempUnschedulable')
  if (row.auto_pause_on_expired && row.expires_at && Date.parse(row.expires_at) <= at) return label('expired')
  return label('unschedulable')
}
</script>
