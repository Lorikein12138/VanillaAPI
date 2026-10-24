import { describe, expect, it } from 'vitest'

import { supportsAccountQuota } from '../accountQuota'

describe('supportsAccountQuota', () => {
  it.each([
    ['service_account', true],
    ['apikey', true],
    ['bedrock', true],
    ['oauth', false],
    ['setup-token', false],
    ['upstream', false],
    [undefined, false],
    [null, false]
  ] as const)('returns %s support as %s', (type, expected) => {
    expect(supportsAccountQuota(type)).toBe(expected)
  })
})
