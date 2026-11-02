// reducedMotion tells whether the system asks for less motion (false where
// matchMedia is missing).
export function reducedMotion(): boolean {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches
}
