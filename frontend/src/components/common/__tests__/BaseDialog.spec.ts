import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import BaseDialog from '../BaseDialog.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

describe('BaseDialog', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    document.body.innerHTML = ''
    document.body.classList.remove('modal-open')
  })

  it('wraps Tab in both directions and restores the opener on close', async () => {
    vi.spyOn(HTMLElement.prototype, 'getClientRects').mockReturnValue([new DOMRect()] as unknown as DOMRectList)
    const opener = document.createElement('button')
    document.body.append(opener)
    opener.focus()
    const wrapper = mount(BaseDialog, {
      props: { show: true, title: 'Details' },
      slots: {
        default: '<input disabled><button>Done</button><input type="radio" name="test" id="last-control" checked><input type="radio" name="test"><button hidden>Hidden</button>',
      },
    })
    try {
      await nextTick()
      const first = document.querySelector('[aria-label="common.closeDialog"]')
      const last = document.getElementById('last-control')
      expect(document.activeElement).toBe(first)
      const pressTab = (shiftKey: boolean) => {
        const event = new KeyboardEvent('keydown', { key: 'Tab', shiftKey, bubbles: true, cancelable: true })
        document.activeElement?.dispatchEvent(event)
        expect(event.defaultPrevented).toBe(true)
      }
      pressTab(true)
      expect(document.activeElement).toBe(last)
      pressTab(false)
      expect(document.activeElement).toBe(first)
      await wrapper.setProps({ show: false })
      expect(document.activeElement).toBe(opener)
    } finally {
      wrapper.unmount()
    }
  })

  it('resets body scroll position when reopened', async () => {
    const wrapper = mount(BaseDialog, {
      attachTo: document.body,
      props: { show: false, title: 'Details' },
      slots: { default: '<div style="height: 2000px">content</div>' },
      global: { stubs: { Icon: true } }
    })

    await wrapper.setProps({ show: true })
    await nextTick()
    const body = document.body.querySelector<HTMLElement>('.modal-body')
    expect(body).not.toBeNull()
    body!.scrollTop = 480

    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await nextTick()

    expect(document.body.querySelector<HTMLElement>('.modal-body')?.scrollTop).toBe(0)
    wrapper.unmount()
  })
})
