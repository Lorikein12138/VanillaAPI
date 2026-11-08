import type { GroupPlatform } from '@/types'

export type KeyGroupProvider = 'anthropic' | 'openai' | 'domestic' | 'other'

export const KEY_GROUP_PROVIDERS = ['anthropic', 'openai', 'domestic', 'other'] as const

// Classify by the configured upstream platform, never by a group's display name.
const PROVIDER_BY_PLATFORM: Record<GroupPlatform, KeyGroupProvider> = {
  anthropic: 'anthropic',
  openai: 'openai',
  kimi: 'domestic',
  zhipu: 'domestic',
  deepseek: 'domestic',
  minimax: 'domestic',
  gemini: 'other',
  grok: 'other',
  antigravity: 'other',
  composite: 'other',
  opencode_go: 'other',
  typesafe: 'other',
  command_code: 'other',
  cline: 'other'
}

export function getKeyGroupProvider(platform: GroupPlatform): KeyGroupProvider {
  return PROVIDER_BY_PLATFORM[platform] ?? 'other'
}

function isKeyGroupProvider(value: unknown): value is KeyGroupProvider {
  return (KEY_GROUP_PROVIDERS as readonly unknown[]).includes(value)
}

/**
 * 分组归类：管理员设置的展示分类优先（如 openai 平台承载的豆包分组归入国产模型），
 * 未设置时按平台自动归类。
 */
export function getGroupProvider(group: {
  platform: string
  display_category?: string | null
}): KeyGroupProvider {
  if (isKeyGroupProvider(group.display_category)) return group.display_category
  return getKeyGroupProvider(group.platform as GroupPlatform)
}

// Collections use representative provider marks rather than an invented brand logo.
export const KEY_GROUP_PROVIDER_ICONS: Record<KeyGroupProvider, GroupPlatform[]> = {
  anthropic: ['anthropic'],
  openai: ['openai'],
  domestic: ['deepseek', 'kimi'],
  other: ['gemini', 'grok']
}
