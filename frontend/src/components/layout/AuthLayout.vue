<template>
  <component :is="inShell ? Passthrough : AuthShell">
    <div class="auth-panel flex min-h-0 flex-col">
      <div class="auth-card min-h-0 overflow-y-auto overscroll-contain rounded-2xl border border-gray-200/90 bg-white p-6 sm:p-10 dark:border-dark-700 dark:bg-dark-800">
        <slot />
      </div>

      <!-- Footer Links -->
      <div class="mt-6 flex-none text-center text-sm">
        <slot name="footer" />
      </div>
    </div>
  </component>
</template>

<script setup lang="ts">
import { inject, type FunctionalComponent } from 'vue'
import AuthShell from './AuthShell.vue'
import { AUTH_SHELL_KEY } from './authShellKey'

// 路由带 meta.authShell 时外壳由 App 常驻提供，这里只出卡片；否则自带外壳独立成页。
const inShell = inject(AUTH_SHELL_KEY, false)
const Passthrough: FunctionalComponent = (_, { slots }) => slots.default?.()
</script>

<style scoped>
.auth-card {
  box-shadow:
    0 1px 2px rgba(20, 20, 19, 0.03),
    0 18px 40px -18px rgba(20, 20, 19, 0.12);
}

:global(.dark) .auth-card {
  box-shadow: 0 18px 40px -18px rgba(0, 0, 0, 0.45);
}

/* 换页时只有卡片轻淡入，外壳与动画不动 */
.auth-panel {
  animation: auth-panel-in 0.36s cubic-bezier(0.22, 1, 0.36, 1) both;
}

@keyframes auth-panel-in {
  from {
    opacity: 0;
    transform: translateY(6px);
  }
}

@media (prefers-reduced-motion: reduce) {
  .auth-panel {
    animation: none;
  }
}
</style>
