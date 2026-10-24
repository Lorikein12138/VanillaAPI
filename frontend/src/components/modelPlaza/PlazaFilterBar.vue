<template>
  <div class="space-y-3">
    <!-- 一级:平台 -->
    <div class="flex items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-medium font-mono uppercase tracking-[0.08em] text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.platformLabel') }}
      </span>
      <div class="flex flex-wrap items-center gap-2">
        <button
          v-for="p in ['all', ...platforms]"
          :key="`platform-${p}`"
          type="button"
          class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-sm font-medium transition disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="p === 'all' ? chipClass(platform === 'all') : platform === p ? 'chip-tinted-active' : 'chip-tinted'"
          :style="p === 'all' ? undefined : { '--chip-accent': platformAccentColor(p) }"
          :disabled="p !== 'all' && !platformEnabled(p)"
          @click="$emit('update:platform', p)"
        >
          <PlatformIcon v-if="p !== 'all'" :platform="p as GroupPlatform" size="xs" />
          {{ p === 'all' ? t('modelPlaza.filters.all') : platformLabel(p) }}
        </button>
      </div>
    </div>

    <!-- 二级:分组(按所属平台着色,当前组合下无结果的置灰) -->
    <div class="flex min-w-0 items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-medium font-mono uppercase tracking-[0.08em] text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.groupLabel') }}
      </span>
      <div class="flex min-w-0 flex-1 items-start gap-2">
        <button
          type="button"
          class="shrink-0 rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="chipClass(groupId === 'all')"
          @click="$emit('update:groupId', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <div class="flex min-w-0 flex-1 flex-col gap-2">
          <div
            v-for="category in groupCategories"
            :key="category.id"
            role="group"
            :aria-label="t(category.labelKey)"
            class="flex min-w-0 flex-col gap-1 sm:flex-row sm:items-start sm:gap-2"
          >
            <span class="text-xs font-medium text-gray-500 dark:text-dark-400 sm:w-20 sm:shrink-0 sm:pt-2">
              {{ t(category.labelKey) }}
            </span>
            <div class="flex min-w-0 flex-wrap items-center gap-2">
              <button
                v-for="g in category.groups"
                :key="`group-${g.id}`"
                type="button"
                class="min-w-0 max-w-full whitespace-normal break-words rounded-lg px-3 py-1.5 text-left text-sm font-medium transition disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
                :class="groupId === g.id ? 'chip-tinted-active' : 'chip-tinted'"
                :style="{ '--chip-accent': platformAccentColor(g.platform) }"
                :disabled="!groupEnabled(g)"
                @click="$emit('update:groupId', g.id)"
              >
                {{ g.name }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 三级:倍率(当前组合下不存在的置灰) -->
    <div class="flex items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-medium font-mono uppercase tracking-[0.08em] text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.rateLabel') }}
      </span>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="chipClass(rate === 'all')"
          @click="$emit('update:rate', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
        </button>
        <button
          v-for="r in rates"
          :key="`rate-${r}`"
          type="button"
          class="rounded-lg px-3 py-1.5 font-mono text-sm font-medium transition disabled:cursor-not-allowed disabled:opacity-40 disabled:grayscale"
          :class="chipClass(rate === r)"
          :disabled="!rateEnabled(r)"
          @click="$emit('update:rate', r)"
        >
          {{ r }}x
        </button>
      </div>
    </div>

    <!-- 四级:模型名搜索(纯前端过滤) -->
    <div class="flex flex-wrap items-start gap-2">
      <span class="w-10 shrink-0 pt-2 text-xs font-medium font-mono uppercase tracking-[0.08em] text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.modelLabel') }}
      </span>
      <div class="relative w-full sm:w-72">
        <Icon
          name="search"
          size="sm"
          class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-dark-500"
        />
        <input
          :value="search"
          type="text"
          :placeholder="t('modelPlaza.filters.searchPlaceholder')"
          class="input rounded-lg py-1.5 pl-9 pr-9"
          @input="$emit('update:search', ($event.target as HTMLInputElement).value)"
        />
        <button
          v-if="search"
          type="button"
          class="absolute right-2.5 top-1/2 -translate-y-1/2 text-gray-400 transition-colors hover:text-gray-600 dark:text-dark-500 dark:hover:text-gray-300"
          @click="$emit('update:search', '')"
        >
          <Icon name="x" size="xs" class="h-3.5 w-3.5" />
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import { platformAccentColor } from '@/utils/platformColors'
import type { GroupPlatform } from '@/types'
import { platformDisplayName } from '@/constants/platformCatalog'
import { getKeyGroupProvider, KEY_GROUP_PROVIDERS } from '@/utils/keyGroupProviders'

const props = defineProps<{
  /** 数据中出现的平台(去重排序后)。 */
  platforms: string[]
  /** 全量分组(含平台与生效倍率),三个维度的置灰联动由此推导。 */
  groups: Array<{ id: number; name: string; platform: string; rate: number }>
  /** 全量生效倍率去重升序。 */
  rates: number[]
  platform: string
  groupId: number | 'all'
  rate: number | 'all'
  /** 模型名搜索词(纯前端过滤)。 */
  search: string
}>()

defineEmits<{
  'update:platform': [value: string]
  'update:groupId': [value: number | 'all']
  'update:rate': [value: number | 'all']
  'update:search': [value: string]
}>()

const { t } = useI18n()

const platformNameKeys: Partial<Record<string, string>> = {
  anthropic: 'modelPlaza.filters.platformNames.anthropic',
  deepseek: 'modelPlaza.filters.platformNames.deepseek',
  openai: 'modelPlaza.filters.platformNames.openai',
  minimax: 'modelPlaza.filters.platformNames.minimax',
  gemini: 'modelPlaza.filters.platformNames.gemini',
  grok: 'modelPlaza.filters.platformNames.grok',
  kimi: 'modelPlaza.filters.platformNames.kimi',
  zhipu: 'modelPlaza.filters.platformNames.zhipu'
}

// 只认自有键：constructor、__proto__ 等原型键不得命中继承成员。
function platformLabel(platform: string): string {
  const key = Object.prototype.hasOwnProperty.call(platformNameKeys, platform)
    ? platformNameKeys[platform]
    : undefined
  return key ? t(key) : platformDisplayName(platform)
}

// 分类结果不在四类之内（如原型键取到继承成员）时归入 other，保证每个分组恰好出现一次。
function groupCategory(platform: string): (typeof KEY_GROUP_PROVIDERS)[number] {
  const provider = getKeyGroupProvider(platform as GroupPlatform)
  return (KEY_GROUP_PROVIDERS as readonly unknown[]).includes(provider) ? provider : 'other'
}

const groupCategories = computed(() =>
  KEY_GROUP_PROVIDERS.map((id) => ({
    id,
    labelKey: `keys.providers.${id}`,
    groups: props.groups.filter((group) => groupCategory(group.platform) === id)
  })).filter((category) => category.groups.length > 0)
)

/**
 * 三个维度互为约束(faceted):某选项可点 ⟺ 在「其他两维」当前选择下仍有分组命中。
 * 「全部」永远可点,作为解除本维约束的出口;可点项组合恒有结果,无需选择修正。
 */
function platformEnabled(p: string): boolean {
  return props.groups.some(
    (g) =>
      g.platform === p &&
      (props.groupId === 'all' || g.id === props.groupId) &&
      (props.rate === 'all' || g.rate === props.rate)
  )
}

function groupEnabled(g: { platform: string; rate: number }): boolean {
  return (
    (props.platform === 'all' || g.platform === props.platform) &&
    (props.rate === 'all' || g.rate === props.rate)
  )
}

function rateEnabled(r: number): boolean {
  return props.groups.some(
    (g) =>
      g.rate === r &&
      (props.platform === 'all' || g.platform === props.platform) &&
      (props.groupId === 'all' || g.id === props.groupId)
  )
}

function chipClass(active: boolean): string {
  return active
    ? 'bg-gray-950 text-gray-50 dark:bg-dark-50 dark:text-gray-950'
    : 'bg-white text-gray-600 ring-1 ring-inset ring-gray-200 enabled:hover:bg-gray-50 enabled:hover:text-gray-900 enabled:hover:ring-gray-300 dark:bg-dark-800/60 dark:text-dark-300 dark:ring-dark-700 dark:enabled:hover:bg-dark-800 dark:enabled:hover:text-white'
}
</script>

<style scoped>
/* 平台/分组 chip 的配色统一从 --chip-accent(平台主色)派生,新增平台无需扩展样式。
   激活态与非激活态在模板上互斥挂载,避免选择器优先级互相覆盖。 */
.chip-tinted {
  color: var(--chip-accent);
  color: color-mix(in srgb, var(--chip-accent) 78%, black);
  background-color: color-mix(in srgb, var(--chip-accent) 9%, transparent);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--chip-accent) 25%, transparent);
}

.chip-tinted:not(:disabled):hover {
  background-color: color-mix(in srgb, var(--chip-accent) 16%, transparent);
}

.dark .chip-tinted {
  color: color-mix(in srgb, var(--chip-accent) 72%, white);
  background-color: color-mix(in srgb, var(--chip-accent) 12%, transparent);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--chip-accent) 30%, transparent);
}

.dark .chip-tinted:not(:disabled):hover {
  background-color: color-mix(in srgb, var(--chip-accent) 18%, transparent);
}

.chip-tinted-active {
  color: #fff;
  background-color: var(--chip-accent);
  background-color: color-mix(in srgb, var(--chip-accent) 85%, black);
  box-shadow: 0 1px 2px 0 color-mix(in srgb, var(--chip-accent) 35%, transparent);
}

.chip-tinted-active:not(:disabled):hover {
  background-color: color-mix(in srgb, var(--chip-accent) 75%, black);
}

.dark .chip-tinted-active {
  background-color: color-mix(in srgb, var(--chip-accent) 80%, transparent);
}

.dark .chip-tinted-active:not(:disabled):hover {
  background-color: var(--chip-accent);
}
</style>
