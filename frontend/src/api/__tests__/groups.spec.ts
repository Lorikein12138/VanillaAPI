import { expect, it, vi } from 'vitest'
import { getAvailable } from '../groups'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../client', () => ({ apiClient: { get } }))

it('sorts available groups by base rate and preserves API order for ties', async () => {
  const data = [
    { id: 1, rate_multiplier: 2, peak_rate_multiplier: 2 },
    { id: 3, rate_multiplier: 0.5, peak_rate_multiplier: 4 },
    { id: 2, rate_multiplier: 0.5, peak_rate_multiplier: 0.5 },
    { id: 4, rate_multiplier: 1, peak_rate_multiplier: 1 }
  ]
  get.mockResolvedValue({ data })

  const groups = await getAvailable()

  expect(get).toHaveBeenCalledWith('/groups/available')
  expect(groups.map(group => group.id)).toEqual([3, 2, 4, 1])
  expect(data.map(group => group.id)).toEqual([1, 3, 2, 4])
})
