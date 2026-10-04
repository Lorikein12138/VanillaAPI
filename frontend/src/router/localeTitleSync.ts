import { onLocaleChange } from '@/i18n'
import router from '@/router'
import { resolveRouteDocumentTitle } from '@/router/title'
import { useAdminSettingsStore } from '@/stores/adminSettings'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'

/**
 * 同步更新浏览器页签标题，使其跟随语言切换。
 * 原先在 `setLocale` 内用动态 import 取 router/stores（为避开 router/title.ts 对 i18n 的反向依赖）；
 * 改由本模块静态导入并向 i18n 注册监听器，i18n 不再引用 router/stores。
 * 必须在 pinia 安装之后调用；返回注销函数。
 */
export function installLocaleTitleSync(): () => void {
  return onLocaleChange(() => {
    const route = router.currentRoute.value
    const appStore = useAppStore()
    const authStore = useAuthStore()
    const adminSettingsStore = useAdminSettingsStore()
    const customMenuItems = [
      ...(appStore.cachedPublicSettings?.custom_menu_items ?? []),
      ...(authStore.isAdmin ? adminSettingsStore.customMenuItems : []),
    ]
    document.title = resolveRouteDocumentTitle(route, appStore.siteName, customMenuItems)
  })
}
