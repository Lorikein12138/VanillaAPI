import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, expect, it, vi } from 'vitest'
import GroupDetectionView from '../GroupDetectionView.vue'

const mocks = vi.hoisted(() => ({
  adminResults: vi.fn(),
  trigger: vi.fn(),
  confirm: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api/channelDetection', () => ({ channelDetectionAPI: mocks }))
vi.mock('@/stores', () => ({
  useAuthStore: () => ({ isAuthenticated: true, isAdmin: true }),
  useAppStore: () => ({ showError: mocks.showError, showSuccess: mocks.showSuccess }),
}))
vi.mock('@/stores/channelDetection', () => ({
  useChannelDetectionStore: () => ({ setPublicEnabled: vi.fn() }),
}))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key, locale: { value: 'en' } }),
}))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))

afterEach(() => vi.resetAllMocks())

it('keeps other idle cards interactive while rendering the SVG and confirmation independently', async () => {
  const svg = '<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>'
  mocks.adminResults.mockResolvedValue([
    {
      group_id: 1, name: 'One', model: 'fixture', trigger_state: 'idle', sample_id: 'sample',
      sample: { checked_at: '2026-11-27T00:00:00Z', pelican_status: 'generated', svg },
      confirmation: { account_name: 'Account', groups: [{ id: 1, name: 'One' }], scope_version: 'scope' },
    },
    { group_id: 2, name: 'Two', trigger_state: 'idle' },
    { group_id: 3, name: 'Three', trigger_state: 'busy' },
  ])
  const pending = new Map<number, () => void>()
  mocks.trigger.mockImplementation((id: number) => new Promise<void>(resolve => pending.set(id, resolve)))
  mocks.confirm.mockResolvedValue({ until: '2026-11-28T00:00:00Z', sync_pending: false })

  const wrapper = mount(GroupDetectionView, {
    global: {
      stubs: {
        Icon: true,
        ConfirmDialog: { props: ['show'], template: '<button v-if="show" @click="$emit(\'confirm\')">confirm</button>' },
      },
    },
  })
  try {
    await flushPromises()
    const [one, two, three] = wrapper.findAll('article')
    expect(one.get('img').attributes('src')).toBe('data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg))
    expect(one.find('dl').exists()).toBe(false)
    expect(three.get('footer button').attributes()).toHaveProperty('disabled')

    await one.get('footer button').trigger('click')
    expect(one.get('footer button').attributes()).toHaveProperty('disabled')
    expect(two.get('footer button').attributes()).not.toHaveProperty('disabled')
    await two.get('footer button').trigger('click')
    expect(mocks.trigger.mock.calls).toEqual([[1], [2]])
    pending.get(2)?.()
    await flushPromises()
    expect(two.get('footer button').attributes()).not.toHaveProperty('disabled')
    expect(one.get('footer button').attributes()).toHaveProperty('disabled')
    pending.get(1)?.()
    await flushPromises()

    await one.findAll('footer button')[1].trigger('click')
    await wrapper.get('main > button').trigger('click')
    await flushPromises()
    expect(mocks.confirm).toHaveBeenCalledWith(1, 'sample', 'scope')
    expect(mocks.showSuccess).toHaveBeenCalledOnce()
  } finally {
    wrapper.unmount()
  }
})
