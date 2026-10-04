import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { getLocale, i18n, onLocaleChange, setLocale } from '@/i18n'

describe('onLocaleChange', () => {
  beforeEach(async () => {
    await setLocale('en')
  })

  afterEach(async () => {
    await setLocale('en')
  })

  it('setLocale 应用新语言后用新语言码调用监听器，且监听器内已能读到新语言', async () => {
    const seen: Array<{ code: string; current: string; lang: string | null }> = []
    const off = onLocaleChange((code) => {
      seen.push({
        code,
        current: i18n.global.locale.value,
        lang: document.documentElement.getAttribute('lang'),
      })
    })

    try {
      await setLocale('zh')
    } finally {
      off()
    }

    expect(seen).toEqual([{ code: 'zh', current: 'zh', lang: 'zh' }])
    expect(getLocale()).toBe('zh')
  })

  it('注销后不再调用监听器，重复注销无副作用', async () => {
    const listener = vi.fn()
    const off = onLocaleChange(listener)

    await setLocale('zh')
    expect(listener).toHaveBeenCalledTimes(1)

    off()
    off()
    await setLocale('en')
    expect(listener).toHaveBeenCalledTimes(1)
  })

  it('监听器按注册顺序依次 await，前一个未完成时后一个不会开始', async () => {
    const order: string[] = []
    let releaseFirst!: () => void
    const firstGate = new Promise<void>((resolve) => {
      releaseFirst = resolve
    })

    const offFirst = onLocaleChange(async () => {
      order.push('first:start')
      await firstGate
      order.push('first:end')
    })
    const offSecond = onLocaleChange(() => {
      order.push('second')
    })

    try {
      const pending = setLocale('zh')
      await vi.waitFor(() => expect(order).toEqual(['first:start']))

      releaseFirst()
      await pending

      expect(order).toEqual(['first:start', 'first:end', 'second'])
    } finally {
      offFirst()
      offSecond()
    }
  })

  it('不支持的语言码被忽略：不切换语言也不调用监听器', async () => {
    const listener = vi.fn()
    const off = onLocaleChange(listener)

    try {
      await setLocale('fr')
    } finally {
      off()
    }

    expect(listener).not.toHaveBeenCalled()
    expect(getLocale()).toBe('en')
  })
})
