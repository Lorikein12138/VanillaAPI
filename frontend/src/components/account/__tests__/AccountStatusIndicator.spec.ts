import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountStatusIndicator from '../AccountStatusIndicator.vue'
import { loadLocaleMessages } from '@/i18n'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params?.time ? `${key}:${params.time}` : key
    })
  }
})

vi.mock('@/utils/format', async () => {
  const actual = await vi.importActual<typeof import('@/utils/format')>('@/utils/format')
  return {
    ...actual,
    formatCountdown: () => '1h',
    formatDateTime: (value: string | null | undefined) => value ? `local:${new Date(value).toLocaleString()}` : ''
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'antigravity',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

describe('AccountStatusIndicator', () => {
  // formatCountdownWithSuffix 等未被 mock 的格式化函数走真实 i18n，需要先载入英文消息，否则 vue-i18n 会向 stderr 报缺键
  beforeAll(async () => {
    await loadLocaleMessages('en')
  })

  afterEach(() => {
    vi.useRealTimers()
  })
  it('Claude 5 系列模型限流时显示 Opus 和 Sonnet 的短别名', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          extra: {
            model_rate_limits: {
              'claude-opus-5': {
                rate_limited_at: '2026-07-28T00:00:00Z',
                rate_limit_reset_at: '2099-07-28T00:00:00Z'
              },
              'claude-sonnet-5': {
                rate_limited_at: '2026-07-28T00:00:00Z',
                rate_limit_reset_at: '2099-07-28T00:00:00Z'
              },
              'claude-sonnet-5-5': {
                rate_limited_at: '2026-09-28T00:00:00Z',
                rate_limit_reset_at: '2099-09-28T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('COpus5')
    expect(wrapper.text()).toContain('CSon5')
    expect(wrapper.text()).toContain('CSon55')
    expect(wrapper.text()).not.toContain('claude-sonnet-5')
  })

  it('ignores a retired health snapshot and keeps the ordinary active status', () => {
    const account = {
      ...makeAccount({}),
      smart_health: { state: 'cooldown', reason: 'ttft', next_probe_at: '2099-07-28T13:00:00Z', backoff_step: 1 }
    } as Account
    const wrapper = mount(AccountStatusIndicator, {
      props: { account },
      global: { stubs: { Icon: true } }
    })
    expect(wrapper.findAll('.badge')).toHaveLength(1)
    expect(wrapper.find('.badge').text()).toBe('admin.accounts.status.active')
    expect(wrapper.text()).not.toContain('smartHealth')
    expect(wrapper.text()).not.toContain('local:')
    wrapper.unmount()
  })

  it('keeps rate limits, overload, errors, temporary suspension and ordinary status badges', () => {
    const cases: { overrides: Partial<Account>; label: string }[] = [
      { overrides: { rate_limit_reset_at: '2099-07-28T13:00:00Z' }, label: 'rateLimited' },
      { overrides: { overload_until: '2099-07-28T13:00:00Z' }, label: 'overloaded' },
      { overrides: { status: 'error' }, label: 'error' },
      { overrides: { temp_unschedulable_until: '2099-07-28T13:00:00Z' }, label: 'tempUnschedulable' },
      { overrides: { status: 'inactive' }, label: 'inactive' },
      { overrides: { quota_used: 100, quota_limit: 100 }, label: 'quotaExceeded' },
      { overrides: { schedulable: false }, label: 'paused' }
    ]
    for (const item of cases) {
      const wrapper = mount(AccountStatusIndicator, {
        props: { account: makeAccount(item.overrides) },
        global: { stubs: { Icon: true } }
      })
      expect(wrapper.findAll('.badge')).toHaveLength(1)
      expect(wrapper.find('.badge').text()).toContain(`admin.accounts.status.${item.label}`)
      expect(wrapper.text()).not.toContain('smartHealth')
      wrapper.unmount()
    }
  })

  it('Grok 账号额度限流时显示自动恢复时间而非临时不可调度', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 5,
          name: 'grok-free-1',
          platform: 'grok',
          rate_limited_at: '2026-07-11T12:00:00Z',
          rate_limit_reset_at: '2099-07-11T13:00:00Z',
          temp_unschedulable_until: '2099-07-11T12:30:00Z',
          temp_unschedulable_reason: 'legacy grok rate limited'
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.find('.badge-warning').text()).toBe('admin.accounts.status.rateLimited')
    expect(wrapper.text()).toContain('admin.accounts.status.rateLimitedAutoResume')
    expect(wrapper.text()).not.toContain('admin.accounts.status.tempUnschedulable')
  })

  it('模型限流 + overages 启用 + 无 AICredits key → 显示 ⚡ (credits_active)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 1,
          name: 'ag-1',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('⚡')
    expect(wrapper.text()).toContain('CSon45')
  })

  it('模型限流 + overages 未启用 → 普通限流样式（无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 2,
          name: 'ag-2',
          extra: {
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
  })

  it('AICredits key 生效 → 显示积分已用尽 (credits_exhausted)', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 3,
          name: 'ag-3',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('admin.accounts.status.creditsExhausted')
  })

  it('模型限流 + overages 启用 + AICredits key 生效 → 普通限流样式（积分耗尽，无 ⚡）', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: makeAccount({
          id: 4,
          name: 'ag-4',
          extra: {
            allow_overages: true,
            model_rate_limits: {
              'claude-sonnet-4-5': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              },
              'AICredits': {
                rate_limited_at: '2026-03-15T00:00:00Z',
                rate_limit_reset_at: '2099-03-15T00:00:00Z'
              }
            }
          }
        })
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    // 模型限流 + 积分耗尽 → 不应显示 ⚡
    expect(wrapper.text()).toContain('CSon45')
    expect(wrapper.text()).not.toContain('⚡')
    // AICredits 积分耗尽状态应显示
    expect(wrapper.text()).toContain('admin.accounts.status.creditsExhausted')
  })
})
