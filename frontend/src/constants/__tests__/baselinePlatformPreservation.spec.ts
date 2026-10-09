import { afterEach, describe, expect, it } from 'vitest'
import { CONCRETE_PLATFORM_OPTIONS, GROUP_PLATFORM_OPTIONS } from '@/constants/platforms'
import {
  BUILTIN_PLATFORM_CATALOG,
  compositePrecedencePlatformIds,
  getPlatformSpec,
  listPlatformIds,
  resetPlatformCatalog,
  setPlatformCatalog,
} from '@/constants/platformCatalog'
import {
  buildModelAllowlistConfig,
  createModelAllowlistSummaryState,
  hydrateModelAllowlistState,
} from '@/views/admin/groupModelAllowlist'

describe('v0.2.15 platform preservation', () => {
  afterEach(() => {
    resetPlatformCatalog()
  })

  it('keeps TypeSafe and composite while deriving options from a new platform catalog entry', () => {
    const typeSafe = getPlatformSpec('typesafe')
    expect(typeSafe?.gateway).toBe('anthropic')
    expect(compositePrecedencePlatformIds()).toContain(typeSafe?.id)
    expect(GROUP_PLATFORM_OPTIONS.map(option => option.value)).toContain('composite')

    const newProvider = {
      id: 'round-trip-provider',
      display_name: 'Round Trip Provider',
      gateway: 'openai' as const,
      cn_provider: false,
    }
    setPlatformCatalog({
      ...BUILTIN_PLATFORM_CATALOG,
      platforms: [...BUILTIN_PLATFORM_CATALOG.platforms, newProvider],
      composite_precedence: [...BUILTIN_PLATFORM_CATALOG.composite_precedence, newProvider.id],
    })

    expect(listPlatformIds()).toContain(newProvider.id)
    expect(CONCRETE_PLATFORM_OPTIONS.map(option => option.value)).toContain(newProvider.id)
    expect(GROUP_PLATFORM_OPTIONS.map(option => option.value)).toContain(newProvider.id)
    expect(compositePrecedencePlatformIds()).toContain(newProvider.id)
    expect(getPlatformSpec('typesafe')?.gateway).toBe('anthropic')
  })

  it('shows automatic source count as response-only metadata', () => {
    const summary = createModelAllowlistSummaryState({
      enabled: true,
      models: ['gpt-5.6-sol'],
      source_account_count: 4,
    })
    expect(summary).toEqual({ models: ['gpt-5.6-sol'], sourceAccountCount: 4 })

    const editable = hydrateModelAllowlistState(
      { enabled: true, models: summary.models },
      summary.models,
    )
    const payload = buildModelAllowlistConfig(editable)
    expect(payload).toEqual({ enabled: true, models: [] })
    expect(payload).not.toHaveProperty('source_account_count')
  })
})
