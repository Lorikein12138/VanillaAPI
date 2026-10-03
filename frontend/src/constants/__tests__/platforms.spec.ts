import { describe, expect, it } from 'vitest'
import { CONCRETE_PLATFORM_OPTIONS, GROUP_PLATFORM_OPTIONS } from '@/constants/platforms'

const concretePlatforms = [
  'anthropic',
  'openai',
  'gemini',
  'antigravity',
  'grok',
  'kimi',
  'zhipu',
  'deepseek',
  'minimax',
  'opencode_go',
  'typesafe'
]

describe('platform option catalogs', () => {
  it('exposes every concrete account platform', () => {
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(concretePlatforms)
  })

  it('adds composite for group-backed filters', () => {
    expect(GROUP_PLATFORM_OPTIONS.map((option) => option.value)).toEqual([
      ...concretePlatforms,
      'composite'
    ])
  })

  it('keeps exactly the supported group platform catalog without Kiro', () => {
    const values = GROUP_PLATFORM_OPTIONS.map((option) => option.value)
    expect(values).toHaveLength(12)
    expect(values).not.toContain('kiro')
    expect(values).toEqual([
      'anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi',
      'zhipu', 'deepseek', 'minimax', 'opencode_go', 'typesafe', 'composite'
    ])
  })
})
