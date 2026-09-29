import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../client'
import { getDimensions, getErrors, getMatrix, getModels, getSnapshot, getUsers, repeatedArrayParamsSerializer } from '../channelMonitorV2'

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

  it('passes one optional as_of anchor through all six V2 read endpoints', async () => {
    const get = vi.spyOn(apiClient, 'get').mockResolvedValue({ data: { items: [] } })
    const filter = { range: '30d', platforms: ['openai'], groupIds: [7], models: ['gpt-5'] }
    const asOf = '2026-09-29T12:34:56.000Z'

    await getDimensions(filter, false, undefined, asOf)
    await getSnapshot(filter, false, undefined, asOf)
    await getMatrix(filter, 'platform_group_model', false, undefined, asOf)
    await getModels(filter, false, undefined, asOf)
    await getErrors(filter, false, undefined, asOf)
    await getUsers(filter, false, undefined, asOf)

    expect(get).toHaveBeenCalledTimes(6)
    for (const [, config] of get.mock.calls) {
      expect(config?.params).toMatchObject({
        range: '30d',
        platform: ['openai'],
        group_id: [7],
        model: ['gpt-5'],
        as_of: asOf,
      })
    }
  })
})
