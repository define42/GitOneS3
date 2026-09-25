import { useSyncExternalStore } from "react";

type ThemePreference = "system" | "light" | "dark";

declare global {
  interface Window {
    gitoneTheme: {
      getPreference: () => ThemePreference;
      setPreference: (value: ThemePreference) => void;
      subscribe: (listener: () => void) => () => void;
    };
  }
}

export function ThemeControl() {
  const preference = useSyncExternalStore(
    window.gitoneTheme.subscribe,
    window.gitoneTheme.getPreference,
  );

  return (
    <label className="theme-control">
      <span>Theme</span>
      <select
        aria-label="Theme"
        value={preference}
        onChange={(event) =>
          window.gitoneTheme.setPreference(event.target.value as ThemePreference)
        }
      >
        <option value="system">System</option>
        <option value="light">Light</option>
        <option value="dark">Dark</option>
      </select>
    </label>
  );
}
