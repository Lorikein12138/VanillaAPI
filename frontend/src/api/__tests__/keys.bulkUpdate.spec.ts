import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { bulkDelete, bulkUpdate } from '../keys'

const { put, remove } = vi.hoisted(() => ({ put: vi.fn(), remove: vi.fn() }))
vi.mock('../client', () => ({ apiClient: { put, delete: remove } }))

describe('API key bulk operations', () => {
  beforeEach(() => {
    put.mockReset()
    remove.mockReset()
  })

  it('updates each selected key once and preserves partial failures', async () => {
    const failure = { response: { status: 403, data: { detail: 'Forbidden' } } }
    put.mockImplementation((url: string) => url === '/keys/2'
      ? Promise.reject(failure)
      : Promise.resolve({ data: { id: Number(url.split('/').pop()) } }))

    const updates = { group_id: 12, quota: 0, expires_at: '', ip_whitelist: [] }
    const result = await bulkUpdate([1, 2, 1, 3], updates)

    expect(put.mock.calls).toEqual([
      ['/keys/1', updates], ['/keys/2', updates], ['/keys/3', updates]
    ])
    expect(result).toEqual({ succeededIds: [1, 3], failures: [{ id: 2, error: failure }] })
  })

  it.each([
    { name: 'updating', request: put, run: (ids: number[]) => bulkUpdate(ids, { status: 'inactive' }) },
    { name: 'deleting', request: remove, run: bulkDelete },
  ])('limits in-flight requests when $name and continues after a failure', async ({ request, run }) => {
    const settle: Array<() => void> = []
    let inFlight = 0
    let peak = 0
    request.mockImplementation(() => new Promise((resolve, reject) => {
      inFlight++
      peak = Math.max(peak, inFlight)
      const index = settle.length
      settle.push(() => {
        inFlight--
        if (index === 0) reject(new Error('network error'))
        else resolve({ data: {} })
      })
    }))

    const pending = run([1, 2, 3, 4, 5, 6, 7, 7])
    expect(request).toHaveBeenCalledTimes(5)
    settle.slice(0, 4).forEach((finish) => finish())
    await flushPromises()
    expect(request).toHaveBeenCalledTimes(5)
    settle[4]()
    await flushPromises()
    expect(request.mock.calls.map(([url]) => url)).toEqual([
      '/keys/1', '/keys/2', '/keys/3', '/keys/4', '/keys/5', '/keys/6', '/keys/7'
    ])
    settle.slice(5).forEach((finish) => finish())
    const result = await pending

    expect(peak).toBe(5)
    expect(result.succeededIds).toEqual([2, 3, 4, 5, 6, 7])
    expect(result.failures.map(({ id }) => id)).toEqual([1])
  })

  it('does not send requests when no keys are selected', async () => {
    expect(await bulkUpdate([], { quota: 0 })).toEqual({ succeededIds: [], failures: [] })
    expect(put).not.toHaveBeenCalled()
  })
})
