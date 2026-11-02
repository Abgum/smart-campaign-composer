import { nextTick, onBeforeUnmount, onMounted, type Ref } from 'vue'

const focusable =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]:not([tabindex="-1"])'

// focusables lists the controls inside el that Tab reaches, in order.
export function focusables(el: HTMLElement | null): HTMLElement[] {
  return el ? [...el.querySelectorAll<HTMLElement>(focusable)] : []
}

// trapTab keeps Tab and Shift+Tab inside el: from the last control Tab goes
// to the first, and back. It returns whether it moved focus.
export function trapTab(e: KeyboardEvent, el: HTMLElement | null): boolean {
  if (e.key !== 'Tab') return false
  const list = focusables(el)
  if (!list.length) return false
  const first = list[0]
  const last = list[list.length - 1]
  const at = document.activeElement
  if (e.shiftKey && (at === first || !el?.contains(at))) {
    e.preventDefault()
    last.focus()
    return true
  }
  if (!e.shiftKey && (at === last || !el?.contains(at))) {
    e.preventDefault()
    first.focus()
    return true
  }
  return false
}

// useDialogFocus makes el behave as a modal dialog for the keyboard: focus
// moves into it when it opens, Tab stays inside, Esc asks to close it, and
// focus returns to where it was when it goes away.
export function useDialogFocus(el: Ref<HTMLElement | null>, close: () => void) {
  let before: HTMLElement | null = null
  // The control marked data-autofocus (a dialog's safe choice) comes first.
  const focusFirst = () =>
    nextTick(() => (el.value?.querySelector<HTMLElement>('[data-autofocus]:not([disabled])') ?? focusables(el.value)[0] ?? el.value)?.focus())

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault()
      close()
      return
    }
    trapTab(e, el.value)
  }

  onMounted(() => {
    before = document.activeElement instanceof HTMLElement ? document.activeElement : null
    void focusFirst()
  })
  onBeforeUnmount(() => before?.focus())
  return { onKey, focusFirst }
}
