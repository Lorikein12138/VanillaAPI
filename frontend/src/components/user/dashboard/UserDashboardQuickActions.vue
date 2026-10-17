<template>
  <div class="card">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h2 class="text-lg font-serif font-normal text-gray-950 dark:text-white">{{ t('dashboard.quickActions') }}</h2>
    </div>
    <div class="space-y-3 p-4">
      <button @click="router.push('/keys')" class="group flex w-full items-center gap-4 rounded-lg border border-gray-200 p-4 text-left transition-colors duration-150 hover:bg-gray-50 dark:border-dark-700 dark:hover:bg-white/5">
        <div class="flex h-12 w-12 flex-shrink-0 items-center justify-center rounded-lg bg-gray-500/10 dark:bg-white/5">
          <Icon name="key" size="lg" class="text-gray-700 dark:text-dark-200" />
        </div>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-gray-900 dark:text-white">{{ t('dashboard.createApiKey') }}</p>
          <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('dashboard.generateNewKey') }}</p>
        </div>
        <Icon
          name="chevronRight"
          size="md"
          class="text-gray-400 transition-colors group-hover:text-primary-500 dark:text-dark-500"
        />
      </button>

      <button @click="router.push('/usage')" class="group flex w-full items-center gap-4 rounded-lg border border-gray-200 p-4 text-left transition-colors duration-150 hover:bg-gray-50 dark:border-dark-700 dark:hover:bg-white/5">
        <div class="flex h-12 w-12 flex-shrink-0 items-center justify-center rounded-lg bg-gray-500/10 dark:bg-white/5">
          <Icon name="chart" size="lg" class="text-gray-700 dark:text-dark-200" />
        </div>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-gray-900 dark:text-white">{{ t('dashboard.viewUsage') }}</p>
          <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('dashboard.checkDetailedLogs') }}</p>
        </div>
        <Icon
          name="chevronRight"
          size="md"
          class="text-gray-400 transition-colors group-hover:text-primary-500 dark:text-dark-500"
        />
      </button>

      <button v-if="canUseBatchImage" @click="router.push('/batch-image')" class="group flex w-full items-center gap-4 rounded-lg border border-gray-200 p-4 text-left transition-colors duration-150 hover:bg-gray-50 dark:border-dark-700 dark:hover:bg-white/5">
        <div class="flex h-12 w-12 flex-shrink-0 items-center justify-center rounded-lg bg-gray-500/10 dark:bg-white/5">
          <Icon name="sparkles" size="lg" class="text-gray-700 dark:text-dark-200" />
        </div>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-gray-900 dark:text-white">{{ t('dashboard.batchImageAgent') }}</p>
          <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('dashboard.batchImageAgentDesc') }}</p>
        </div>
        <Icon
          name="chevronRight"
          size="md"
          class="text-gray-400 transition-colors group-hover:text-primary-500 dark:text-dark-500"
        />
      </button>

      <button @click="router.push('/redeem')" class="group flex w-full items-center gap-4 rounded-lg border border-gray-200 p-4 text-left transition-colors duration-150 hover:bg-gray-50 dark:border-dark-700 dark:hover:bg-white/5">
        <div class="flex h-12 w-12 flex-shrink-0 items-center justify-center rounded-lg bg-gray-500/10 dark:bg-white/5">
          <Icon name="gift" size="lg" class="text-gray-700 dark:text-dark-200" />
        </div>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-gray-900 dark:text-white">{{ t('dashboard.redeemCode') }}</p>
          <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('dashboard.addBalanceWithCode') }}</p>
        </div>
        <Icon
          name="chevronRight"
          size="md"
          class="text-gray-400 transition-colors group-hover:text-primary-500 dark:text-dark-500"
        />
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { useBatchImageAccess } from '@/composables/useBatchImageAccess'
const router = useRouter()
const { t } = useI18n()
const { canUseBatchImage, refreshBatchImageAccess } = useBatchImageAccess()

onMounted(() => {
  void refreshBatchImageAccess()
})
</script>
