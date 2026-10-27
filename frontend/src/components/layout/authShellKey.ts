import type { InjectionKey } from 'vue'

/** AuthShell 向下提供：AuthLayout 处在常驻外壳内时只渲染卡片，不再重复套外壳 */
export const AUTH_SHELL_KEY: InjectionKey<boolean> = Symbol('auth-shell')
