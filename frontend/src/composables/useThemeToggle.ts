import { ref } from 'vue'

const THEME_STORAGE_KEY = 'theme'

function readIsDark(): boolean {
  if (typeof document === 'undefined') return false
  return document.documentElement.classList.contains('dark')
}

/**
 * Shared light/dark toggle. The theme class itself is applied before mount in main.ts;
 * this only flips it and persists the choice.
 */
export function useThemeToggle() {
  const isDark = ref(readIsDark())

  function setTheme(dark: boolean) {
    isDark.value = dark
    document.documentElement.classList.toggle('dark', dark)
    try {
      localStorage.setItem(THEME_STORAGE_KEY, dark ? 'dark' : 'light')
    } catch {
      // storage unavailable (private mode); the class change still applies
    }
  }

  function toggleTheme() {
    setTheme(!readIsDark())
  }

  return { isDark, setTheme, toggleTheme }
}
