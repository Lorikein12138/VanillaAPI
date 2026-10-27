<template>
  <div class="auth-shell relative isolate flex min-h-screen flex-col overflow-x-hidden bg-gray-50 text-gray-950 sm:h-[100dvh] sm:min-h-0 lg:flex-row dark:bg-dark-950 dark:text-dark-50">
    <!-- 丝绸动画直接画在页面底上：宽屏铺左半屏向右渐隐，窄屏铺整页向下渐隐 -->
    <div class="auth-field pointer-events-none absolute inset-0 -z-10 lg:right-auto lg:w-[70%]">
      <AuthSilkField :dark="isDark" />
    </div>

    <!-- Editorial pane -->
    <section class="hidden min-w-0 flex-[1.12_1_0%] flex-col justify-between px-14 py-11 lg:flex xl:px-20">
      <router-link to="/" class="flex w-fit items-center gap-2.5">
        <template v-if="settingsLoaded">
          <span class="inline-flex h-8 w-8 flex-shrink-0 items-center justify-center overflow-hidden rounded-md">
            <img :src="siteLogo || '/logo.svg'" alt="Logo" class="h-full w-full object-contain" />
          </span>
          <span class="text-[1.0625rem] font-medium tracking-tight">{{ siteName }}</span>
        </template>
      </router-link>

      <div v-if="settingsLoaded" class="max-w-xl pb-6">
        <h1 class="auth-display font-serif font-normal">
          {{ siteName }}
        </h1>
        <p class="mt-6 max-w-md text-[1.0625rem] leading-relaxed text-gray-700 dark:text-dark-200">
          {{ siteSubtitle }}
        </p>
        <div class="mt-9 h-px w-16 bg-primary-500/70"></div>
      </div>

      <p class="text-xs text-gray-600 dark:text-dark-300">
        &copy; {{ currentYear }} {{ siteName }}
      </p>
    </section>

    <!-- Form pane -->
    <div class="flex min-h-0 min-w-0 flex-1 flex-col px-4 pb-6 pt-4 sm:px-8 lg:px-12 lg:pb-8 lg:pt-11 xl:px-16">
      <header class="flex flex-none items-center justify-between gap-4">
        <router-link to="/" class="flex min-w-0 items-center lg:hidden" :aria-label="siteName">
          <span v-if="settingsLoaded" class="inline-flex h-8 w-8 flex-shrink-0 items-center justify-center overflow-hidden rounded-md">
            <img :src="siteLogo || '/logo.svg'" alt="Logo" class="h-full w-full object-contain" />
          </span>
        </router-link>
        <button
          type="button"
          class="ml-auto inline-flex h-9 w-9 flex-shrink-0 items-center justify-center rounded-full text-gray-600 transition-colors hover:bg-gray-500/10 hover:text-gray-950 dark:text-dark-300 dark:hover:bg-white/5 dark:hover:text-dark-50"
          :aria-label="isDark ? t('home.switchToLight') : t('home.switchToDark')"
          :title="isDark ? t('home.switchToLight') : t('home.switchToDark')"
          @click="toggleTheme"
        >
          <Icon v-if="isDark" name="sun" size="md" />
          <Icon v-else name="moon" size="md" />
        </button>
      </header>

      <main class="flex min-h-0 flex-1 flex-col items-center justify-center py-8 sm:py-6">
        <!-- 卡片高度受视口约束（sm 及以上），内容过长时卡片内滚动，页面不出现滚动条 -->
        <div class="flex w-full min-w-0 max-w-[25.5rem] flex-col sm:max-h-full">
          <div v-if="settingsLoaded" class="mb-8 flex-none px-1 lg:hidden">
            <h1 class="auth-display font-serif font-normal">{{ siteName }}</h1>
            <p class="mt-3 text-[0.9375rem] leading-relaxed text-gray-700 dark:text-dark-200">{{ siteSubtitle }}</p>
          </div>
          <slot />
        </div>
      </main>

      <!-- Copyright -->
      <footer class="flex-none text-center text-xs text-gray-500 lg:hidden dark:text-dark-400">
        &copy; {{ currentYear }} {{ siteName }}
      </footer>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, provide } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import AuthSilkField from '@/components/auth/AuthSilkField.vue'
import { useAppStore } from '@/stores'
import { useThemeToggle } from '@/composables/useThemeToggle'
import { sanitizeUrl } from '@/utils/url'
import { AUTH_SHELL_KEY } from './authShellKey'

// 认证页外壳：由 App 按路由 meta.authShell 常驻挂载，页面之间跳转时丝绸动画与左屏不重建，
// 只有 AuthLayout 渲染的卡片随路由切换。
provide(AUTH_SHELL_KEY, true)

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

<style scoped>
.auth-field {
  -webkit-mask-image: linear-gradient(180deg, #000 0%, rgba(0, 0, 0, 0.85) 40%, transparent 92%);
  mask-image: linear-gradient(180deg, #000 0%, rgba(0, 0, 0, 0.85) 40%, transparent 92%);
}

@media (min-width: 1024px) {
  .auth-field {
    -webkit-mask-image: linear-gradient(90deg, #000 0%, #000 36%, rgba(0, 0, 0, 0.5) 64%, transparent 96%);
    mask-image: linear-gradient(90deg, #000 0%, #000 36%, rgba(0, 0, 0, 0.5) 64%, transparent 96%);
  }
}

.auth-display {
  font-size: clamp(2.75rem, 5.6vw, 5.25rem);
  line-height: 1;
  letter-spacing: -0.025em;
}
</style>

<style>
/* 编辑风衬线：拉丁字形走 Newsreader，中文落回系统衬线/黑体 */
@font-face {
  font-family: 'Newsreader';
  font-style: normal;
  font-weight: 400;
  font-display: swap;
  src: url('../../assets/fonts/newsreader-400.woff2') format('woff2');
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}

.auth-shell .font-serif {
  font-family: 'Newsreader', Georgia, 'Noto Serif SC', 'Source Han Serif SC', 'Songti SC', 'PingFang SC', 'Microsoft YaHei', serif;
}

.auth-card h2.font-serif {
  font-size: 1.875rem;
  line-height: 1.2;
  letter-spacing: -0.015em;
}

.auth-card .input {
  border-radius: 0.625rem;
  padding-top: 0.6875rem;
  padding-bottom: 0.6875rem;
}

.auth-card .btn:not(.btn-sm) {
  border-radius: 0.625rem;
  padding-top: 0.75rem;
  padding-bottom: 0.75rem;
}
</style>
