export function getTabStops(root: HTMLElement, backward = false): HTMLElement[] {
  const candidates = [...root.querySelectorAll<HTMLElement>(
    'a[href], button, input, select, textarea, [tabindex], [contenteditable="true"]'
  )].filter(element => element.tabIndex >= 0 && !element.matches(':disabled') &&
    !element.closest('[hidden], [inert]') && element.getClientRects().length > 0 &&
    getComputedStyle(element).visibility === 'visible')

  // Positive tabindex precedes the natural DOM order.
  candidates.sort((a, b) => (a.tabIndex || Infinity) - (b.tabIndex || Infinity))
  const radios = candidates.filter((element): element is HTMLInputElement =>
    element instanceof HTMLInputElement && element.type === 'radio' && element.name !== '')
  return candidates.filter(element => {
    if (!(element instanceof HTMLInputElement) || element.type !== 'radio' || !element.name) return true
    const group = radios.filter(radio => radio.name === element.name && radio.form === element.form)
    return element === (group.find(radio => radio === document.activeElement) ??
      group.find(radio => radio.checked) ?? (backward ? group.at(-1) : group[0]))
  })
}

export function containTab(event: KeyboardEvent, root: HTMLElement) {
  const stops = getTabStops(root, event.shiftKey)
  const first = stops[0]
  const last = stops.at(-1)
  const active = document.activeElement
  if (!stops.some(element => element === active) || active === (event.shiftKey ? first : last)) {
    event.preventDefault()
    const target = (event.shiftKey ? last : first) ?? root
    target.focus()
  }
}
