<template>
  <AppLayout>
    <div v-if="!enabled" class="card space-y-3 p-5">
      <p v-if="checkingSettings" role="status">{{ t('common.loading') }}</p>
      <template v-else>
        <p role="status">{{ label(adminSettings.gatewayAdaptiveSchedulingEnabled === false ? 'disabled' : 'settingsLoadFailed') }}</p>
        <button type="button" class="btn btn-secondary" @click="checkSettings">{{ t('common.refresh') }}</button>
      </template>
    </div>
    <TablePageLayout v-else>
      <template #filters>
        <div class="flex flex-col gap-3 xl:flex-row xl:items-end xl:justify-between">
          <div class="flex min-w-0 flex-1 flex-wrap items-end gap-3">
            <div class="w-full max-w-full sm:w-56">
              <label for="account-scores-group" class="input-label">{{ label('chooseGroup') }}</label>
              <Select
                id="account-scores-group"
                :aria-label="label('chooseGroup')"
                :model-value="selectedGroup?.value ?? null"
                :options="groupOptions"
                :placeholder="label('chooseGroup')"
                :search-placeholder="t('admin.groups.searchGroups')"
                :loading="groupsLoading"
                remote
                clearable
                @change="selectGroup"
                @search="searchGroups"
              >
                <template #selected>{{ selectedGroup?.label ?? label('chooseGroup') }}</template>
              </Select>
            </div>
            <div v-if="groupPages > 1" class="flex items-center gap-2">
              <button type="button" class="btn btn-secondary btn-sm" :disabled="groupPage <= 1 || groupsLoading" @click="loadGroups(groupPage - 1)">
                {{ t('pagination.previous') }}
              </button>
              <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('pagination.pageOf', { page: groupPage, total: groupPages }) }}</span>
              <button type="button" class="btn btn-secondary btn-sm" :disabled="groupPage >= groupPages || groupsLoading" @click="loadGroups(groupPage + 1)">
                {{ t('pagination.next') }}
              </button>
            </div>
            <div class="w-full min-w-0 sm:w-72 lg:w-80">
              <label for="account-scores-dimension" class="input-label">{{ label('dimension') }}</label>
              <Select
                id="account-scores-dimension"
                :aria-label="label('dimension')"
                :model-value="selectedDimension?.value ?? null"
                :options="dimensionOptions"
                :placeholder="label('noDimension')"
                :disabled="dimensionOptions.length === 0"
                @change="selectDimension"
              >
                <template #selected>
                  <span :title="selectedDimension?.label">{{ selectedDimension?.label ?? label('noDimension') }}</span>
                </template>
                <template #option="{ option, selected }">
                  <span class="min-w-0 flex-1 whitespace-normal [overflow-wrap:anywhere]">{{ option.label }}</span>
                  <Icon v-if="selected" name="check" size="sm" class="shrink-0 text-primary-500" />
                </template>
              </Select>
            </div>
          </div>
          <div class="flex flex-wrap gap-2">
            <button type="button" class="btn btn-secondary" :disabled="checkingSettings || scoresLoading" @click="selectedGroup ? loadScores() : checkSettings()">
              <Icon name="refresh" size="md" class="mr-2" />{{ t('common.refresh') }}
            </button>
            <button type="button" class="btn btn-secondary" :disabled="!selectedGroup" @click="autoRefresh.setEnabled(!autoEnabled)">
              {{ label(autoEnabled ? 'pause' : 'resume') }}
            </button>
          </div>
        </div>
        <p v-if="groupsError" class="mt-2 text-sm text-red-600 dark:text-red-400" role="status">
          {{ label('groupLoadFailed') }}
          <button type="button" class="btn btn-ghost btn-sm" @click="loadGroups(groupPage)">{{ t('common.refresh') }}</button>
        </p>
        <p v-if="snapshot" class="mt-2 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.accountScores.latestSnapshot', { time: date(snapshot.observed_at) }) }}
        </p>
        <div v-if="snapshot?.items.length" class="mt-1 flex items-center text-xs text-gray-500 dark:text-gray-400 md:hidden">
          <span>{{ label('overallScore') }}</span>
          <HelpTooltip :content="label('overallHint')" trigger="click" width-class="w-48" />
        </div>
        <p v-if="snapshot?.items.length && dimensionOptions.length === 0" class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ label('noSamples') }}
        </p>
        <p v-if="scoresError" class="mt-2 text-sm text-red-600 dark:text-red-400" role="status">{{ label(scoresError) }}</p>
      </template>

      <template #table>
        <DataTable
          v-if="selectedGroup && (snapshot || scoresLoading)"
          :columns="columns"
          :data="tableRows"
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
          <template #cell-metric="{ row }">
            <span v-if="row.v2Bucket" :class="['badge text-xs', healthClass(row.v2Bucket.state)]">
              {{ label('state.' + row.v2Bucket.state) }}
            </span>
            <span v-else-if="row.v1Bucket && !row.v1Bucket.known" class="badge badge-gray text-xs">
              {{ label(row.v1Bucket.sample_count === 0 && row.v1Bucket.health ? 'state.' + row.v1Bucket.health.state : 'insufficient') }}
            </span>
            <span v-else class="font-medium tabular-nums">
              {{ row.v1Bucket?.score_kind === 'general' ? reliability(row.v1Bucket) : row.v1Bucket?.latency ?? '—' }}
            </span>
          </template>
          <template #cell-secondary="{ row }">
            <span class="tabular-nums">
              {{ row.v1Bucket?.score_kind === 'text_stream' ? reliability(row.v1Bucket) : row.v1Bucket?.latency ?? '—' }}
            </span>
          </template>
          <template #cell-samples="{ row }">
            <span class="tabular-nums">{{ row.v1Bucket?.sample_count ?? row.v2Bucket?.sample_count ?? '—' }}</span>
          </template>
          <template #cell-actions="{ row }">
            <button
              type="button"
              class="btn btn-ghost btn-sm"
              aria-haspopup="dialog"
              @click="expandedId = row.id"
            >
              {{ label('details') }}
            </button>
          </template>
        </DataTable>
        <p v-else-if="!selectedGroup" class="p-6 text-sm text-gray-500 dark:text-gray-400">{{ label('chooseGroup') }}</p>

      </template>

      <template #pagination>
        <p v-if="snapshot && snapshot.total > memberTotal" class="mb-2 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.accountScores.memberPageLimit', { accessible: memberTotal, total: snapshot.total }) }}
        </p>
        <Pagination
          v-if="selectedGroup && memberTotal > 0"
          :page="memberPage"
          :page-size="memberPageSize"
          :total="memberTotal"
          :show-page-size-selector="false"
          @update:page="changeMemberPage"
        />
      </template>
    </TablePageLayout>
    <BaseDialog :show="!!expanded" :title="label('detailTitle')" width="wide" @close="expandedId = null">
        <section v-if="expanded" id="account-score-details" class="min-w-0 space-y-4">
          <h2 class="break-all text-base font-semibold text-gray-900 dark:text-white">{{ expanded.name }}</h2>
          <p class="text-sm text-gray-500 dark:text-gray-400">{{ label('scope') }}</p>
          <div class="grid gap-5 lg:grid-cols-2">
            <div class="min-w-0">
              <h3 class="font-medium">{{ label('v1') }}</h3>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ label('v1Window') }}</p>
              <p v-if="expanded.v1.length === 0" class="mt-2 text-sm">{{ label('noSamples') }}</p>
              <ul v-else class="mt-2 divide-y divide-gray-200 dark:divide-dark-700">
                <li v-for="(bucket, index) in expanded.v1" :key="index" class="space-y-2 py-3">
                  <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span class="break-all font-medium">{{ bucket.model || '—' }}</span>
                    <span v-if="bucket.health" :class="['badge text-xs', healthClass(bucket.health.state)]">{{ label('state.' + bucket.health.state) }}</span>
                  </div>
                  <p class="text-xs text-gray-500 [overflow-wrap:anywhere] dark:text-gray-400">
                    {{ dimensions(bucket) }} / {{ label('work.' + bucket.workload) }}
                  </p>
                  <dl class="grid grid-cols-2 gap-x-3 gap-y-1 text-sm">
                    <dt>{{ label('samples') }}</dt><dd>{{ bucket.sample_count }}</dd>
                    <dt>{{ label('lastSample') }}</dt><dd>{{ date(bucket.latest_sample_at) }}</dd>
                    <dt>{{ label('scoreWindow') }}</dt><dd>{{ t('admin.accountScores.minutes', { value: bucket.score_window_minutes }) }}</dd>
                    <dt>{{ label(bucket.score_kind === 'text_stream' ? 'reliabilityRate' : 'reliabilityBand') }}</dt>
                    <dd>{{ reliability(bucket) }}</dd>
                    <dt>{{ label(bucket.score_kind === 'text_stream' ? 'firstOutputPoints' : 'latencyBand') }}</dt>
                    <dd>{{ bucket.latency ?? '—' }}</dd>
                    <template v-if="bucket.health">
                      <dt>{{ label('healthUpdated') }}</dt><dd>{{ date(bucket.health.updated_at) }}</dd>
                      <dt v-if="bucket.health.next_retest_at">{{ label('nextRetest') }}</dt>
                      <dd v-if="bucket.health.next_retest_at">{{ date(bucket.health.next_retest_at) }}</dd>
                    </template>
                  </dl>
                  <p v-if="!bucket.known" class="text-xs text-gray-500 dark:text-gray-400">
                    {{ label(bucket.sample_count === 0 && bucket.health ? 'healthOnly' : 'insufficient') }}
                  </p>
                </li>
              </ul>
              <p v-if="expanded.v1_truncated" class="text-xs text-gray-500 dark:text-gray-400">{{ label('showingRecent') }}</p>
            </div>
            <div class="min-w-0">
              <h3 class="font-medium">{{ label('v2') }}</h3>
              <p class="text-xs text-gray-500 dark:text-gray-400">{{ label('v2Window') }}. {{ label('v2Coverage') }}</p>
              <p v-if="!snapshot?.v2_ready" class="mt-2 text-sm">{{ label('v2Starting') }}</p>
              <p v-if="expanded.v2.length === 0" class="mt-2 text-sm">{{ label('noSamples') }}</p>
              <ul v-else class="mt-2 divide-y divide-gray-200 dark:divide-dark-700">
                <li v-for="(bucket, index) in expanded.v2" :key="index" class="space-y-2 py-3">
                  <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span class="break-all font-medium">{{ bucket.model || '—' }}</span>
                    <span :class="['badge text-xs', healthClass(bucket.state)]">{{ label('state.' + bucket.state) }}</span>
                  </div>
                  <p class="text-xs text-gray-500 [overflow-wrap:anywhere] dark:text-gray-400">
                    {{ dimensions(bucket) }} / {{ bucket.wire }} / {{ bucket.workloads.map((work) => label('work.' + work)).join(', ') }}
                  </p>
                  <dl class="grid grid-cols-2 gap-x-3 gap-y-1 text-sm">
                    <dt>{{ label('samples') }}</dt><dd>{{ bucket.sample_count }}</dd>
                    <dt>{{ label('lastSample') }}</dt><dd>{{ date(bucket.latest_sample_at) }}</dd>
                    <dt>{{ label('firstOutputMs') }}</dt>
                    <dd>{{ bucket.latest_first_output_ms === null ? '—' : t('admin.accountScores.milliseconds', { value: bucket.latest_first_output_ms }) }}</dd>
                  </dl>
                </li>
              </ul>
              <p v-if="expanded.v2_truncated" class="text-xs text-gray-500 dark:text-gray-400">{{ label('showingRecent') }}</p>
            </div>
          </div>
        </section>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api'
import type { AdaptiveAccountScoresPage, AdaptiveScoreDimensions, AdaptiveScoreMember, AdaptiveScoreV1, AdaptiveScoreV2 } from '@/api/admin/groups'
import BaseDialog from '@/components/common/BaseDialog.vue'
import DataTable from '@/components/common/DataTable.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import type { Column } from '@/components/common/types'
import Icon from '@/components/icons/Icon.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { useAdminSettingsStore } from '@/stores/adminSettings'

const { t, locale } = useI18n()
const label = (key: string) => t(`admin.accountScores.${key}`)
const adminSettings = useAdminSettingsStore()
const settingsChecked = ref(false)
const checkingSettings = ref(true)
const enabled = computed(() => settingsChecked.value && adminSettings.gatewayAdaptiveSchedulingEnabled === true)
const groupOptions = ref<SelectOption[]>([])
const selectedGroup = ref<SelectOption | null>(null)
const groupPage = ref(1)
const groupPages = ref(0)
const groupsLoading = ref(false)
const groupsError = ref(false)
let groupSearch = ''
let groupsRequest: AbortController | null = null

const snapshot = shallowRef<AdaptiveAccountScoresPage | null>(null)
const memberPage = ref(1)
const memberPageSize = 20
const memberTotal = ref(0)
const scoresLoading = ref(false)
const scoresError = ref<'refreshFailed' | 'missingGroup' | null>(null)
const expandedId = ref<number | null>(null)
const expanded = computed(() => snapshot.value?.items.find((member) => member.id === expandedId.value))
let scoresRequest: AbortController | null = null

type ScoreBucket = AdaptiveScoreV1 | AdaptiveScoreV2
type ScoreDimension = SelectOption & { value: string; version: 'v1' | 'v2'; kind?: AdaptiveScoreV1['score_kind'] }
const selectedDimension = shallowRef<ScoreDimension | null>(null)
const availableDimensions = computed(() => collectDimensions(snapshot.value?.items ?? []))
const dimensionOptions = computed(() => {
  const current = selectedDimension.value
  const options = availableDimensions.value
  return current && !options.some((option) => option.value === current.value) ? [current, ...options] : options
})
const columns = computed<Column[]>(() => {
  const dimension = selectedDimension.value
  const v2 = dimension?.version === 'v2'
  const textStream = dimension?.kind === 'text_stream'
  const columns: Column[] = [
    { key: 'name', label: label('member') },
    { key: 'overall', label: label('overallScore'), class: 'text-right' },
    { key: 'status', label: label('schedulability') },
    { key: 'metric', label: label(v2 ? 'streamState' : textStream ? 'firstOutputRange' : dimension ? 'reliabilityBand' : 'score'), class: v2 ? '' : 'text-right' },
  ]
  if (dimension?.version === 'v1') {
    columns.push({ key: 'secondary', label: label(textStream ? 'reliabilityRate' : 'latencyBand'), class: 'text-right' })
  }
  columns.push({ key: 'samples', label: label('samples'), class: 'text-right' }, { key: 'actions', label: label('detailTitle') })
  return columns
})
const tableRows = computed(() => {
  const dimension = selectedDimension.value
  return (snapshot.value?.items ?? []).map((member) => ({
    ...member,
    v1Bucket: dimension?.version === 'v1' ? member.v1.find((bucket) => bucketKey(bucket) === dimension.value) : undefined,
    v2Bucket: dimension?.version === 'v2' ? member.v2.find((bucket) => bucketKey(bucket) === dimension.value) : undefined,
  }))
})

const autoRefresh = useAutoRefresh({
  storageKey: 'admin-account-scores-refresh',
  intervals: [15],
  defaultInterval: 15,
  defaultEnabled: true,
  onRefresh: loadScores,
  shouldPause: () => document.hidden || !enabled.value || !selectedGroup.value || scoresLoading.value,
})
const autoEnabled = autoRefresh.enabled

function restartCountdown(seconds: number) {
  autoRefresh.intervalSeconds.value = seconds
  autoRefresh.resetCountdown()
}

async function loadGroups(page = 1) {
  if (!enabled.value) return
  groupsRequest?.abort()
  const request = new AbortController()
  groupsRequest = request
  groupsLoading.value = true
  groupsError.value = false
  try {
    const result = await adminAPI.groups.list(page, 20, { search: groupSearch || undefined }, { signal: request.signal })
    if (groupsRequest !== request || !enabled.value) return
    groupOptions.value = result.items.map((group) => ({ value: group.id, label: group.name }))
    groupPage.value = result.page
    groupPages.value = result.pages
  } catch {
    if (groupsRequest === request && enabled.value) groupsError.value = true
  } finally {
    if (groupsRequest === request) {
      groupsRequest = null
      groupsLoading.value = false
    }
  }
}

function searchGroups(search: string) {
  groupSearch = search.trim()
  groupOptions.value = []
  groupPage.value = 1
  groupPages.value = 0
  void loadGroups()
}

function selectGroup(value: SelectOption['value'], option: SelectOption | null) {
  if (selectedGroup.value?.value === value) return
  selectedGroup.value = typeof value === 'number' ? option : null
  selectedDimension.value = null
  memberPage.value = 1
  memberTotal.value = 0
}

function clearScores() {
  scoresRequest?.abort()
  scoresRequest = null
  scoresLoading.value = false
  scoresError.value = null
  expandedId.value = null
  snapshot.value = null
  restartCountdown(15)
}

function changeMemberPage(page: number) {
  if (page >= 1 && page <= Math.ceil(memberTotal.value / memberPageSize)) memberPage.value = page
}

async function loadScores() {
  const groupId = selectedGroup.value?.value
  if (!enabled.value || typeof groupId !== 'number' || scoresRequest) return
  const request = new AbortController()
  scoresRequest = request
  scoresLoading.value = true
  const page = memberPage.value
  const isCurrent = () => scoresRequest === request && enabled.value && selectedGroup.value?.value === groupId
    && memberPage.value === page
  try {
    const result = await adminAPI.groups.getAdaptiveAccountScores(groupId, page, memberPageSize, request.signal)
    if (!isCurrent()) return
    snapshot.value = result
    selectedDimension.value ??= availableDimensions.value[0] ?? null
    memberTotal.value = result.max_queryable_total
    scoresError.value = null
    restartCountdown(15)
  } catch (error) {
    if (!isCurrent()) return
    const failure = error as { status?: number; reason?: string } | null
    if (failure?.reason === 'ADAPTIVE_ACCOUNT_SCORES_DISABLED') {
      adminSettings.setGatewayAdaptiveSchedulingEnabled(false)
      return
    }
    if (failure?.status === 404) {
      scoresError.value = 'missingGroup'
      snapshot.value = null
      memberTotal.value = 0
    } else {
      scoresError.value = 'refreshFailed'
    }
    restartCountdown(Math.min(60, autoRefresh.intervalSeconds.value * 2))
  } finally {
    if (scoresRequest === request) {
      scoresRequest = null
      scoresLoading.value = false
    }
  }
}

watch([() => selectedGroup.value?.value, memberPage], () => {
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
  selectedGroup.value = null
  selectedDimension.value = null
  groupSearch = ''
  groupPage.value = 1
  groupPages.value = 0
  memberTotal.value = 0
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
const percentFormatter = computed(() => new Intl.NumberFormat(locale.value, { style: 'percent', maximumFractionDigits: 2 }))
const date = (at: string | null) => at ? dateFormatter.value.format(new Date(at)) : '—'

function dimensions(bucket: AdaptiveScoreDimensions) {
  return [
    bucket.operation, bucket.transport,
    bucket.tier === 'default' ? label('defaultTier') : bucket.tier,
    bucket.effort && `${label('effort')} ${bucket.effort}`,
    `${label('inputSize')} ${bucket.input_size}`,
    bucket.duration && `${label('duration')} ${bucket.duration}`,
    (bucket.thinking_level || bucket.thinking_enabled) && `${label('thinking')} ${bucket.thinking_level || t('common.enabled')}`,
  ].filter(Boolean).join(' / ')
}

function reliability(bucket: AdaptiveScoreV1) {
  if (bucket.reliability === null) return '—'
  if (bucket.score_kind === 'text_stream') return percentFormatter.value.format(bucket.reliability / 100)
  return `${bucket.reliability}/5`
}

function bucketKey(bucket: ScoreBucket) {
  const version = 'wire' in bucket ? 'v2' : 'v1'
  return JSON.stringify([
    version, bucket.model, bucket.operation, bucket.transport, bucket.tier, bucket.effort,
    bucket.input_size, bucket.duration ?? '', bucket.thinking_level ?? '', bucket.thinking_enabled,
    'wire' in bucket ? [bucket.wire, ...bucket.workloads] : [bucket.score_kind, bucket.workload],
  ])
}

function collectDimensions(members: AdaptiveScoreMember[]): ScoreDimension[] {
  const seen = new Map<string, { option: ScoreDimension; rank: number; at: number }>()
  for (const member of members) {
    for (const bucket of [...member.v1, ...member.v2]) {
      const v2 = 'wire' in bucket
      const value = bucketKey(bucket)
      const at = Date.parse(bucket.latest_sample_at ?? (v2 ? '' : bucket.health?.updated_at ?? '')) || 0
      // Prefer a recent bucket that can display a v1 score. v2 state and
      // insufficient-sample buckets remain selectable from the same control.
      const rank = v2 ? Number(bucket.state !== 'insufficient') : 2 * Number(bucket.known)
      const existing = seen.get(value)
      if (existing) {
        existing.rank = Math.max(existing.rank, rank)
        existing.at = Math.max(existing.at, at)
        continue
      }
      const version = v2 ? 'v2' : 'v1'
      const workload = v2 ? bucket.workloads.map((work) => label('work.' + work)).join(', ') : label('work.' + bucket.workload)
      const title = [version, bucket.model || '—', dimensions(bucket), v2 && bucket.wire, workload].filter(Boolean).join(' / ')
      seen.set(value, { option: { value, label: title, version, kind: v2 ? undefined : bucket.score_kind }, rank, at })
    }
  }
  return [...seen.values()].sort((a, b) => b.rank - a.rank
    || b.at - a.at || a.option.value.localeCompare(b.option.value)).map(({ option }) => option)
}

function selectDimension(value: SelectOption['value']) {
  selectedDimension.value = dimensionOptions.value.find((option) => option.value === value) ?? null
}

function healthClass(state: string) {
  if (['healthy', 'normal', 'recovered'].includes(state)) return 'badge-success'
  return state === 'insufficient' ? 'badge-gray' : 'badge-warning'
}

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
