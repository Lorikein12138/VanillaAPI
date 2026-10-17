<template>
  <div class="flex min-h-screen flex-col bg-gray-50 text-gray-950 dark:bg-dark-950 dark:text-dark-50">
    <!-- Top bar -->
    <header class="border-b border-gray-200 dark:border-dark-700">
      <div class="mx-auto flex max-w-6xl items-center justify-between gap-4 px-4 py-4 sm:px-8">
        <router-link to="/" class="flex min-w-0 items-center gap-2.5">
          <template v-if="settingsLoaded">
            <span class="inline-flex h-8 w-8 flex-shrink-0 items-center justify-center overflow-hidden rounded-md">
              <img :src="siteLogo || '/logo.svg'" alt="Logo" class="h-full w-full object-contain" />
            </span>
            <span class="truncate text-[1.0625rem] font-medium">{{ siteName }}</span>
          </template>
        </router-link>
        <button
          type="button"
          class="inline-flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-lg border border-gray-300 text-gray-600 transition-colors hover:bg-gray-500/10 hover:text-gray-950 dark:border-dark-600 dark:text-dark-300 dark:hover:bg-white/5 dark:hover:text-dark-50"
          :aria-label="isDark ? t('home.switchToLight') : t('home.switchToDark')"
          :title="isDark ? t('home.switchToLight') : t('home.switchToDark')"
          @click="toggleTheme"
        >
          <Icon v-if="isDark" name="sun" size="md" />
          <Icon v-else name="moon" size="md" />
        </button>
      </div>
    </header>

    <!-- Content -->
    <main class="mx-auto flex w-full max-w-6xl flex-1 flex-wrap items-center justify-center gap-x-20 gap-y-10 px-4 py-10 sm:px-8 sm:py-14">
      <!-- Editorial intro -->
      <section v-if="settingsLoaded" class="hidden min-w-0 max-w-md flex-[1_1_360px] flex-col gap-6 lg:flex">
        <h1 class="font-serif text-[2.75rem] font-normal leading-[1.2] tracking-tight">
          {{ siteName }}
        </h1>
        <p class="text-[1.0625rem] leading-relaxed text-gray-600 dark:text-dark-300">
          {{ siteSubtitle }}
        </p>
        <div class="h-px w-24 bg-primary-500/60"></div>
      </section>

      <!-- Card Container -->
      <div class="w-full min-w-0 max-w-[26rem] flex-[1_1_360px]">
        <div class="rounded-xl border border-gray-200 bg-white p-6 sm:p-9 dark:border-dark-700 dark:bg-dark-800">
          <slot />
        </div>

        <!-- Footer Links -->
        <div class="mt-6 text-center text-sm">
          <slot name="footer" />
        </div>
      </div>
    </main>

    <!-- Copyright -->
    <footer class="px-4 pb-8 text-center text-xs text-gray-500 dark:text-dark-400">
      &copy; {{ currentYear }} {{ siteName }}. All rights reserved.
    </footer>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores'
import { useThemeToggle } from '@/composables/useThemeToggle'
import { sanitizeUrl } from '@/utils/url'

const { t } = useI18n()
const appStore = useAppStore()
const { isDark, toggleTheme } = useThemeToggle()

const siteName = computed(() => appStore.siteName || 'Sub2API')
const siteLogo = computed(() => sanitizeUrl(appStore.siteLogo || '', { allowRelative: true, allowDataUrl: true }))
const siteSubtitle = computed(() => appStore.cachedPublicSettings?.site_subtitle || 'Subscription to API Conversion Platform')
const settingsLoaded = computed(() => appStore.publicSettingsLoaded)

const currentYear = computed(() => new Date().getFullYear())

onMounted(() => {
  appStore.fetchPublicSettings()
})
</script>
