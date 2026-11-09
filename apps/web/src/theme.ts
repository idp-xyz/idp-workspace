import { useEffect, useState } from 'react';

// Light or dark, the member's choice kept in this browser; "system" follows the OS.
export type Theme = 'light' | 'dark' | 'system';
const KEY = 'idp-workspace.theme';

export function resolve(theme: Theme): 'light' | 'dark' {
  if (theme !== 'system') return theme;
  return typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(() => (localStorage.getItem(KEY) as Theme | null) ?? 'system');
  useEffect(() => {
    document.documentElement.dataset.theme = resolve(theme);
    localStorage.setItem(KEY, theme);
    if (theme !== 'system' || !window.matchMedia) return;
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = () => { document.documentElement.dataset.theme = resolve('system'); };
    mq.addEventListener?.('change', onChange);
    return () => mq.removeEventListener?.('change', onChange);
  }, [theme]);
  return [theme, setTheme];
}
