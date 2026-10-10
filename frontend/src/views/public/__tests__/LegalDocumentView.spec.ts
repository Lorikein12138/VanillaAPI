import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, RouterLinkStub } from '@vue/test-utils'
import LegalDocumentView from '../LegalDocumentView.vue'

const state = vi.hoisted(() => ({
  locale: 'zh',
  route: { params: { documentId: 'admin-compliance' } },
  appStore: {
    cachedPublicSettings: { site_name: 'Test site', login_agreement_documents: [] },
    fetchPublicSettings: vi.fn().mockResolvedValue({})
  }
}))

vi.mock('vue-router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-router')>()
  return { ...actual, useRoute: () => state.route }
})
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key === 'adminCompliance.title'
        ? (state.locale === 'zh' ? '部署与运营合规承诺' : 'Deployment and Operation Compliance Commitment')
        : key
    })
  }
})
vi.mock('@/i18n', () => ({ getLocale: () => state.locale }))
vi.mock('@/stores/app', () => ({ useAppStore: () => state.appStore }))

afterEach(() => vi.clearAllMocks())

function mountPage() {
  return mount(LegalDocumentView, {
    global: {
      stubs: {
        RouterLink: RouterLinkStub,
        Icon: { template: '<span data-testid="icon" />' }
      }
    }
  })
}

describe('LegalDocumentView admin compliance source', () => {
  it.each([
    ['zh', '部署与运营合规承诺', '本文件适用于部署、配置、管理、运营或实际控制 Sub2API 实例'],
    ['en', 'Deployment and Operation Compliance Commitment', 'This document applies to any individual, organization']
  ])('renders the %s local agreement body', async (locale, title, body) => {
    state.locale = locale
    const wrapper = mountPage()
    await flushPromises()

    expect(wrapper.get('article h1').text()).toBe(title)
    expect(wrapper.get('.legal-document-content').text()).toContain(body)
    expect(wrapper.get('.legal-document-content h2').exists()).toBe(true)
  })
})
