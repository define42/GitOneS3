import { useEffect, useId, useRef, useState } from "react";
import { api, errorMessage } from "./api";
import type { Session } from "./api";
import { ThemeControl } from "./ThemeControl";
import "./header.css";

export function Header({ session }: { session: Session | null }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const accountRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const accountNavigationId = useId();
  const username = session?.username ?? "";

  useEffect(() => {
    if (!open) return;

    function closeOutside(event: PointerEvent | FocusEvent) {
      if (!accountRef.current?.contains(event.target as Node)) setOpen(false);
    }
    function closeOnEscape(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      event.preventDefault();
      setOpen(false);
      triggerRef.current?.focus();
    }

    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("focusin", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("focusin", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  async function logout() {
    if (!session || busy) return;
    setBusy(true);
    setError("");
    try {
      await api("/logout", { method: "POST", csrf: session.csrfToken });
      window.location.assign("/auth/login?signedOut=1");
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
      setOpen(false);
      triggerRef.current?.focus();
    }
  }

  return (
    <>
      <a className="skip-link" href="#main">
        Skip to main content
      </a>
      <header className="site-header">
        <a href="/" className="site-brand">
          <svg
            width="28"
            height="28"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.7"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <circle cx="6" cy="5" r="3" />
            <circle cx="6" cy="19" r="3" />
            <circle cx="18" cy="5" r="3" />
            <path d="M6 8v8m12-8a8 8 0 0 1-8 8H6" />
          </svg>
          <span>GitOne</span>
        </a>
        <ThemeControl />
        {session?.authenticated ? (
          <div className="account-disclosure" ref={accountRef}>
            <button
              className="account-trigger"
              type="button"
              ref={triggerRef}
              aria-label={`Account: ${username}`}
              aria-expanded={open}
              aria-controls={accountNavigationId}
              onClick={() => setOpen(!open)}
            >
              <span className="account-avatar" aria-hidden="true">
                {username.slice(0, 2).toUpperCase()}
              </span>
              <span className="account-trigger-name">{username}</span>
              <svg
                width="14"
                height="14"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <path d="m6 9 6 6 6-6" />
              </svg>
            </button>
            <nav
              className="account-menu"
              id={accountNavigationId}
              aria-label="Account navigation"
              hidden={!open}
            >
              <p className="account-menu-identity">
                Signed in as <strong>{username}</strong>
              </p>
              <a href="/">Your spaces</a>
              <a href="/auth/tokens">Settings</a>
              <div className="account-menu-signout">
                <button type="button" onClick={logout} disabled={busy}>
                  {busy ? "Signing out…" : "Sign out"}
                </button>
              </div>
            </nav>
          </div>
        ) : session ? (
          <nav className="site-auth-actions" aria-label="Account">
            <a href="/auth/login">Sign in</a>
            <a className="site-register" href="/auth/register">
              Create account
            </a>
          </nav>
        ) : null}
      </header>
      {error && (
        <div className="container">
          <div className="notice error" role="alert">
            {error}
          </div>
        </div>
      )}
    </>
  );
}
