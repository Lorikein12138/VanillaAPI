import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { setLocale } from '@/i18n'
import { installLocaleTitleSync } from '@/router/localeTitleSync'

const state = vi.hoisted(() => ({
  route: { name: 'Dashboard', params: {}, meta: { titleKey: 'nav.dashboard' } } as {
    name: string
    params: Record<string, string>
    meta: Record<string, unknown>
  },
  siteName: 'My Site',
  publicMenu: [] as Array<{ id: string; label: string }>,
  isAdmin: false,
  adminMenu: [] as Array<{ id: string; label: string }>,
}))

vi.mock('@/router', () => ({
  default: {
    currentRoute: {
      get value() {
        return state.route
      },
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    siteName: state.siteName,
    cachedPublicSettings: { custom_menu_items: state.publicMenu },
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isAdmin: state.isAdmin }),
}))

vi.mock('@/stores/adminSettings', () => ({
  useAdminSettingsStore: () => ({ customMenuItems: state.adminMenu }),
}))

describe('installLocaleTitleSync', () => {
  let uninstall: () => void

  beforeEach(async () => {
    state.route = { name: 'Dashboard', params: {}, meta: { titleKey: 'nav.dashboard' } }
    state.siteName = 'My Site'
    state.publicMenu = []
    state.isAdmin = false
    state.adminMenu = []
    await setLocale('en')
    document.title = ''
    uninstall = installLocaleTitleSync()
  })

  afterEach(async () => {
    uninstall()
    await setLocale('en')
  })

  it('切换语言后页签标题按新语言重新生成', async () => {
    await setLocale('zh')
    expect(document.title).toBe('仪表盘 - My Site')

    await setLocale('en')
    expect(document.title).toBe('Dashboard - My Site')
  })

  it('站点名取自 app store', async () => {
    state.siteName = 'Lori API'

    await setLocale('zh')

    expect(document.title).toBe('仪表盘 - Lori API')
  })

  it('自定义页面标题使用公开设置里的菜单名称', async () => {
    state.route = { name: 'CustomPage', params: { id: 'pub-1' }, meta: { titleKey: 'nav.dashboard' } }
    state.publicMenu = [{ id: 'pub-1', label: 'Public Guide' }]

    await setLocale('zh')

    expect(document.title).toBe('Public Guide - My Site')
  })

  it('管理员才会合并 adminSettings 里的自定义菜单项', async () => {
    state.route = { name: 'CustomPage', params: { id: 'adm-1' }, meta: { titleKey: 'nav.dashboard' } }
    state.adminMenu = [{ id: 'adm-1', label: 'Admin Only Page' }]

    await setLocale('zh')
    // 非管理员：找不到菜单项，回退到路由 titleKey 的译文
    expect(document.title).toBe('仪表盘 - My Site')

    state.isAdmin = true
    await setLocale('en')
    expect(document.title).toBe('Admin Only Page - My Site')
  })

  it('注销后切换语言不再改写页签标题', async () => {
    uninstall()
    document.title = 'unchanged'

    await setLocale('zh')

    expect(document.title).toBe('unchanged')
  })
})
