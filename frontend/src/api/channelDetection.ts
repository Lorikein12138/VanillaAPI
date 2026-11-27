import { apiClient } from './client'

export interface DetectionSelection {
  group_id: number
  model: string
  api_key_id: number
}

export interface DetectionConfig {
  enabled: boolean
  interval_minutes: number
  groups: DetectionSelection[]
}

export interface DetectionCounts {
  passed: number
  failed: number
  errors: number
  unattributed: number
  untested: number
}

export interface DetectionSample {
  checked_at: string
  reasoning: DetectionCounts
  fingerprint: DetectionCounts
  pelican_status: 'generated' | 'error' | 'untested'
  svg?: string
}

export interface PublicDetectionGroup {
  name: string
  model?: string
  sample?: DetectionSample
}

const adminPath = '/admin/channel-detection'
const publicPath = '/channel-detection'

export const channelDetectionAPI = {
  async config() {
    return (await apiClient.get<DetectionConfig>(adminPath)).data
  },
  async save(config: DetectionConfig) {
    await apiClient.put(adminPath, config)
  },
  async groups() {
    return (await apiClient.get<{ id: number; name: string }[]>(adminPath + '/groups')).data
  },
  async models(groupID: number) {
    return (await apiClient.get<string[]>(`${adminPath}/groups/${groupID}/models`)).data
  },
  async publicEnabled() {
    return (await apiClient.get<{ enabled: boolean }>(publicPath + '/status')).data.enabled
  },
  async results(signal?: AbortSignal) {
    return (await apiClient.get<PublicDetectionGroup[]>(publicPath, { signal })).data
  },
}
