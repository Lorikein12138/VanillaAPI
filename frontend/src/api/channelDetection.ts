import { apiClient } from './client'

export interface DetectionSelection {
  group_id: number
  model: string
}

export interface DetectionGroupOption {
  id: number
  name: string
  active: boolean
  has_models: boolean
  mode_restricted: boolean
  claude_code_only: boolean
}

export interface DetectionConfig {
  enabled: boolean
  interval_minutes: number
  concurrent_groups: number
  groups: DetectionSelection[]
}

export interface DetectionSample {
  checked_at: string
  pelican_status: 'generated' | 'error' | 'untested'
  svg?: string
}

export interface PublicDetectionGroup {
  name: string
  model?: string
  sample?: DetectionSample
}

export interface DetectionConfirmationScope {
  account_name: string
  groups: { id: number; name: string }[]
  scope_version: string
}

export interface AdminDetectionGroup extends PublicDetectionGroup {
  group_id: number
  trigger_state: 'idle' | 'running' | 'busy' | 'no_send'
  sample_id?: string
  confirmation?: DetectionConfirmationScope
}

export interface DetectionConfirmation {
  until: string
  sync_pending: boolean
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
    return (await apiClient.get<DetectionGroupOption[]>(adminPath + '/groups')).data
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
  async adminResults(signal?: AbortSignal) {
    return (await apiClient.get<AdminDetectionGroup[]>(adminPath + '/results', { signal })).data
  },
  async trigger(groupID: number) {
    return (await apiClient.post<{ sample_id: string }>(`${adminPath}/groups/${groupID}/trigger`)).data
  },
  async confirm(groupID: number, sampleID: string, scopeVersion: string) {
    return (await apiClient.post<DetectionConfirmation>(
      `${adminPath}/groups/${groupID}/samples/${sampleID}/confirm`,
      { scope_version: scopeVersion },
    )).data
  },
}
