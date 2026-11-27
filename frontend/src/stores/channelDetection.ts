import { defineStore } from 'pinia'
import { ref } from 'vue'
import { channelDetectionAPI } from '@/api/channelDetection'

export const useChannelDetectionStore = defineStore('channelDetection', () => {
  const publicEnabled = ref(false)
  let pending: Promise<void> | undefined
  let revision = 0

  function setPublicEnabled(enabled: boolean) {
    revision++
    publicEnabled.value = enabled
  }

  function refresh(force = false) {
    if (pending && !force) return pending
    const requested = ++revision
    const request = channelDetectionAPI.publicEnabled()
      .then(enabled => {
        if (requested === revision) publicEnabled.value = enabled
      })
      .catch(() => {
        if (requested === revision) publicEnabled.value = false
      })
      .finally(() => {
        if (pending === request) pending = undefined
      })
    pending = request
    return request
  }

  return { publicEnabled, refresh, setPublicEnabled }
})
