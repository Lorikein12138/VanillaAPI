import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, expect, it, vi } from 'vitest'
import GroupDetectionView from '../GroupDetectionView.vue'

const mocks = vi.hoisted(() => ({
  adminResults: vi.fn(),
  results: vi.fn(),
  administrator: true,
  translate: vi.fn((key: string, _values?: unknown) => key),
  trigger: vi.fn(),
  confirm: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}))

vi.mock('@/api/channelDetection', () => ({ channelDetectionAPI: mocks }))
vi.mock('@/stores', () => ({
  useAuthStore: () => ({ isAuthenticated: true, isAdmin: mocks.administrator }),
  useAppStore: () => ({ showError: mocks.showError, showSuccess: mocks.showSuccess }),
}))
vi.mock('@/stores/channelDetection', () => ({
  useChannelDetectionStore: () => ({ setPublicEnabled: vi.fn() }),
}))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: mocks.translate, locale: { value: 'en' } }),
}))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))

afterEach(() => {
  vi.resetAllMocks()
  mocks.translate.mockImplementation(key => key)
  mocks.administrator = true
})

it('keeps other idle cards interactive while rendering the SVG and confirmation independently', async () => {
  const svg = '<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>'
  mocks.adminResults.mockResolvedValue([
    {
      group_id: 1, name: 'One', model: 'fixture', trigger_state: 'idle', sample_id: 'sample',
      sample: { checked_at: '2026-11-27T00:00:00Z', pelican_status: 'generated', svg },
      latest: { checked_at: '2026-11-27T02:00:00Z', pelican_status: 'error' },
      confirmation: { account_name: 'Account', groups: [{ id: 1, name: 'One' }], scope_version: 'scope', suspend_minutes: 45 },
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
    expect(one.get('time').attributes('datetime')).toBe('2026-11-27T00:00:00Z')
    expect(one.get('footer').text()).toContain('channelDetection.latestResult')
    expect(two.text()).toContain('channelDetection.noSample')
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
    expect(mocks.translate).toHaveBeenCalledWith('channelDetection.confirmMessage', {
      account: 'Account', minutes: 45, group: 'One', time: expect.any(String),
    })
    await wrapper.get('main > button').trigger('click')
    await flushPromises()
    expect(mocks.confirm).toHaveBeenCalledWith(1, 'sample', 'scope')
    expect(mocks.showSuccess).toHaveBeenCalledOnce()
  } finally {
    wrapper.unmount()
  }
})

it('dismisses a confirmation when the latest result is replaced without changing the display image', async () => {
  const sample = { checked_at: '2026-11-27T00:00:00Z', pelican_status: 'generated', svg: '<svg><circle r="1"/></svg>' }
  const group = {
    group_id: 1, name: 'One', model: 'fixture', trigger_state: 'idle',
    sample, sample_id: 'old',
    latest: { checked_at: '2026-11-27T01:00:00Z', pelican_status: 'error' },
    confirmation: { account_name: 'Account', groups: [], scope_version: 'scope', suspend_minutes: 30 },
  }
  const other = { group_id: 2, name: 'Two', trigger_state: 'idle' }
  mocks.adminResults.mockResolvedValueOnce([group, other]).mockResolvedValueOnce([
    { ...group, sample_id: 'new', latest: { checked_at: '2026-11-27T02:00:00Z', pelican_status: 'untested' }, confirmation: undefined },
    other,
  ])
  mocks.trigger.mockResolvedValue(undefined)
  const wrapper = mount(GroupDetectionView, {
    global: {
      stubs: {
        Icon: true,
        ConfirmDialog: { props: ['show'], template: '<div v-if="show" role="dialog" />' },
      },
    },
  })
  try {
    await flushPromises()
    const [one, two] = wrapper.findAll('article')
    await one.findAll('footer button')[1].trigger('click')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(true)
    await two.get('footer button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(one.get('time').attributes('datetime')).toBe(sample.checked_at)
    expect(one.get('img').exists()).toBe(true)
    expect(mocks.showError).toHaveBeenCalledWith('channelDetection.confirmationChanged')
    expect(mocks.confirm).not.toHaveBeenCalled()
  } finally {
    wrapper.unmount()
  }
})

it('shows only the no-sample message when a public card has no image', async () => {
  mocks.administrator = false
  mocks.results.mockResolvedValue([{ name: 'One', model: 'fixture' }])
  const wrapper = mount(GroupDetectionView)
  try {
    await flushPromises()
    const card = wrapper.get('article')
    expect(card.text()).toContain('channelDetection.noSample')
    expect(card.find('img, time, footer').exists()).toBe(false)
    expect(mocks.adminResults).not.toHaveBeenCalled()
  } finally {
    wrapper.unmount()
  }
})

it('shows legacy no-send attempts to administrators with a time when available', async () => {
  const checkedAt = '2026-11-27T02:00:00Z'
  const legacy = { trigger_state: 'no_send', latest: { pelican_status: 'untested' } }
  mocks.adminResults.mockResolvedValue([
    { ...legacy, group_id: 1, name: 'One', latest: { ...legacy.latest, checked_at: checkedAt } },
    { ...legacy, group_id: 2, name: 'Two' },
  ])
  const wrapper = mount(GroupDetectionView, { global: { stubs: { Icon: true } } })
  try {
    await flushPromises()
    const [timed, untimed] = wrapper.findAll('article')
    expect(timed.get('footer').text()).toContain('channelDetection.latestResult')
    expect(mocks.translate).toHaveBeenCalledWith('channelDetection.latestResult', {
      time: new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(checkedAt)),
      status: 'channelDetection.noSend',
    })
    expect(untimed.get('footer').text()).toContain('channelDetection.noSend')
    for (const card of [timed, untimed]) {
      expect(card.text()).toContain('channelDetection.noSample')
      expect(card.findAll('footer button')).toHaveLength(1)
    }
  } finally {
    wrapper.unmount()
  }
})
