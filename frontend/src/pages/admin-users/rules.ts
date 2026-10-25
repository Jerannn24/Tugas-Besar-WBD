// Validasi UX sesuai C03; server tetap sumber kebenaran (422 VALIDATION_FAILED).
export const USERNAME_MIN = 3
export const USERNAME_MAX = 64
export const PASSWORD_MIN = 12
const PASSWORD_MAX_BYTES = 1024

export function usernameError(value: string): string | null {
  const length = [...value.trim()].length
  if (length === 0) return 'Username wajib diisi.'
  if (length < USERNAME_MIN || length > USERNAME_MAX) {
    return `Username harus ${USERNAME_MIN} sampai ${USERNAME_MAX} karakter.`
  }
  return null
}

export function passwordError(value: string): string | null {
  if (value.length === 0) return 'Password wajib diisi.'
  if ([...value].length < PASSWORD_MIN) return `Password minimal ${PASSWORD_MIN} karakter.`
  if (new TextEncoder().encode(value).length > PASSWORD_MAX_BYTES) return 'Password maksimal 1 KiB.'
  return null
}
