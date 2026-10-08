import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../client'
import {
  getConfig,
  getDimensions,
  getErrors,
  getMatrix,
  getModels,
  getSnapshot,
  getUsers,
  type MonitorConfig,
  repeatedArrayParamsSerializer,
  updateConfig,
} from '../channelMonitorV2'

afterEach(() => vi.restoreAllMocks())

describe('channel monitor V2 query serialization', () => {
  it('uses repeated keys without bracket suffixes for array filters', () => {
    const query = repeatedArrayParamsSerializer({
      range: '90m',
      platform: ['openai', 'grok'],
      group_id: [1, 2],
      model: undefined,
      group_by: 'platform_group_model',
    })

    expect(query).toBe('range=90m&platform=openai&platform=grok&group_id=1&group_id=2&group_by=platform_group_model')
    expect(query).not.toContain('%5B%5D')
  })

  it('sends the matrix grouping with the shared filters', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({
      data: { coverage: {}, group_by: 'platform_group', items: [] },
    })

    await getMatrix({ range: '24h', platforms: ['openai'], groupIds: [7], models: [] }, 'platform_group', true)

    expect(get).toHaveBeenCalledWith('/admin/channel-monitor-v2/matrix', expect.objectContaining({
      params: {
        range: '24h',
        platform: ['openai'],
        group_id: [7],
        model: undefined,
        group_by: 'platform_group',
      },
    }))
  })

  it('omits retired as_of from all six original user reads', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ data: { items: [] } })
    const filter = { range: '30d', platforms: ['openai'], groupIds: [7], models: ['gpt-5'] }

    await getDimensions(filter, false, undefined)
    await getSnapshot(filter, false, undefined)
    await getMatrix(filter, 'platform_group_model', false, undefined)
    await getModels(filter, false, undefined)
    await getErrors(filter, false, undefined)
    await getUsers(filter, false, undefined)

    expect(get).toHaveBeenCalledTimes(6)
    for (const [, config] of get.mock.calls) {
      expect(config?.params).not.toHaveProperty('as_of')
      expect(config?.params).toMatchObject({
        range: '30d',
        platform: ['openai'],
        group_id: [7],
        model: ['gpt-5'],
      })
    }
  })

  it('uses only original admin routes for all six status resources', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ data: { items: [] } })
    const filter = { range: '90m' as const, platforms: [], groupIds: [], models: [] }

    await getDimensions(filter, true)
    await getSnapshot(filter, true)
    await getMatrix(filter, 'platform_group', true)
    await getModels(filter, true)
    await getErrors(filter, true)
    await getUsers(filter, true)

    expect(get.mock.calls.map(([path]) => path)).toEqual([
      '/admin/channel-monitor-v2/dimensions',
      '/admin/channel-monitor-v2/snapshot',
      '/admin/channel-monitor-v2/matrix',
      '/admin/channel-monitor-v2/models',
      '/admin/channel-monitor-v2/errors',
      '/admin/channel-monitor-v2/users',
    ])
  })

  it('keeps configuration on the separate raw admin config route', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ data: {} })
    await getConfig()
    expect(get).toHaveBeenCalledWith('/admin/channel-monitor-v2/config')

    const put = vi.spyOn(apiClient, 'put').mockResolvedValue({ data: {} })
    const config = { version: 4 } as MonitorConfig
    await updateConfig(config)
    expect(put).toHaveBeenCalledWith('/admin/channel-monitor-v2/config', config)
  })
})
