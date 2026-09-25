import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage } from "./api";
import type { Session, SSHKey } from "./api";
import { SettingsNavigation } from "./SettingsNavigation";

function dateLabel(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleDateString();
}

export function SSHKeysPage({ session }: { session: Session }) {
  const endpoint = `/users/${encodeURIComponent(session.username!)}/ssh-keys`;
  const [keys, setKeys] = useState<SSHKey[] | null>(null);
  const [refresh, setRefresh] = useState(0);
  const [listError, setListError] = useState("");
  const [status, setStatus] = useState("");
  const [showForm, setShowForm] = useState(false);
  const [name, setName] = useState("");
  const [publicKey, setPublicKey] = useState("");
  const [nameError, setNameError] = useState("");
  const [keyError, setKeyError] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [revoking, setRevoking] = useState("");
  const nameRef = useRef<HTMLInputElement>(null);
  const errorRef = useRef<HTMLDivElement>(null);
  const addRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    let active = true;
    setListError("");
    api<{ keys: SSHKey[] }>(endpoint)
      .then((result) => {
        if (active) setKeys(result.keys);
      })
      .catch((e) => {
        if (active) setListError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, [endpoint, refresh]);
  useEffect(() => {
    if (showForm) nameRef.current?.focus();
  }, [showForm]);
  useEffect(() => {
    if (error || nameError || keyError) errorRef.current?.focus();
  }, [error, nameError, keyError]);

  function closeForm() {
    setShowForm(false);
    setName("");
    setPublicKey("");
    setError("");
    setNameError("");
    setKeyError("");
    // The existing button stays mounted, so keyboard users keep their place.
    requestAnimationFrame(() => addRef.current?.focus());
  }

  async function add(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    setStatus("");
    const nextNameError = !name.trim()
      ? "Enter a name for this key."
      : new TextEncoder().encode(name.trim()).length > 100
        ? "Use a key name of at most 100 bytes."
        : "";
    const value = publicKey.trim();
    const nextKeyError = /PRIVATE KEY/.test(value)
      ? "This looks like a private key. Keep it on your device and paste the contents of the .pub file instead."
      : new TextEncoder().encode(value).length > 4096
        ? "Use a public key line of at most 4096 bytes."
        : !/^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521)) [A-Za-z0-9+/=]+(?: [^\r\n]*)?$/.test(
              value,
            )
          ? "Paste one public key line from your .pub file, without options or line breaks."
          : "";
    setNameError(nextNameError);
    setKeyError(nextKeyError);
    if (nextNameError || nextKeyError) {
      errorRef.current?.focus();
      return;
    }
    setBusy(true);
    try {
      const result = await api<{ key: SSHKey }>(endpoint, {
        method: "POST",
        csrf: session.csrfToken,
        body: { name: name.trim(), publicKey: value },
      });
      setKeys((current) => [result.key, ...(current ?? [])]);
      closeForm();
      setStatus(
        session.sshURL
          ? `“${result.key.name}” was added. You can now use it to authenticate Git over SSH.`
          : `“${result.key.name}” was added. It will be available when an administrator enables SSH.`,
      );
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function revoke(key: SSHKey) {
    if (
      !window.confirm(
        `Revoke “${key.name}”? Git clients using this key will lose access. This key cannot be added again; generate a new key if needed.`,
      )
    )
      return;
    setRevoking(key.id);
    setListError("");
    setStatus("");
    try {
      await api(`${endpoint}/${encodeURIComponent(key.id)}`, {
        method: "DELETE",
        csrf: session.csrfToken,
      });
      setKeys(
        (current) =>
          current?.map((item) =>
            item.id === key.id
              ? { ...item, revokedAt: new Date().toISOString() }
              : item,
          ) ?? null,
      );
      setStatus(`“${key.name}” was revoked.`);
    } catch (e) {
      setListError(errorMessage(e));
    } finally {
      setRevoking("");
    }
  }

  return (
    <main id="main" className="container token-page">
      <a className="back-link" href="/">
        ← Your spaces
      </a>
      <div className="page-heading">
        <div>
          <p className="eyebrow">PERSONAL SETTINGS</p>
          <h1>SSH keys</h1>
          <p className="muted">
            Connect your devices to GitOne without a password or access token.
          </p>
        </div>
        <button
          ref={addRef}
          className="button primary"
          disabled={showForm || !keys || keys.length >= 100}
          onClick={() => {
            setShowForm(true);
            setStatus("");
          }}
        >
          Add SSH key
        </button>
      </div>
      <div className="settings-layout">
        <aside className="settings-aside">
          <SettingsNavigation active="ssh-keys" />
          <div className="role-guide">
            <h2>One key per device</h2>
            <p>
              Only your public key is stored here. Keep your private key on your
              device and protect it with a passphrase.
            </p>
            <p>
              Keys identify your account. Your current personal and group
              permissions decide which repositories you can read or write.
            </p>
            <p>
              SSH supports Git operations only, not an interactive shell or file
              transfer.
            </p>
          </div>
        </aside>
        <div className="settings-content token-content">
          {status && (
            <div className="notice success" role="status">
              {status}
            </div>
          )}
          {!session.sshURL && (
            <div className="notice" role="status">
              SSH is not enabled on this server. You can register keys now, but
              use HTTPS until an administrator enables SSH.
            </div>
          )}
          {showForm && (
            <section aria-label="Add SSH key">
              <h2>Add a new SSH key</h2>
              <form
                className="panel form-panel ssh-key-form"
                onSubmit={add}
                noValidate
              >
                {(error || nameError || keyError) && (
                  <div
                    ref={errorRef}
                    className="notice error"
                    role="alert"
                    tabIndex={-1}
                  >
                    <h3>Unable to add key</h3>
                    {error && <p>{error}</p>}
                    {nameError && (
                      <p>
                        <a href="#ssh-key-name">{nameError}</a>
                      </p>
                    )}
                    {keyError && (
                      <p>
                        <a href="#ssh-public-key">{keyError}</a>
                      </p>
                    )}
                  </div>
                )}
                <label htmlFor="ssh-key-name">Key name</label>
                <input
                  ref={nameRef}
                  id="ssh-key-name"
                  required
                  maxLength={100}
                  autoComplete="off"
                  value={name}
                  disabled={busy}
                  onChange={(event) => setName(event.target.value)}
                  aria-invalid={Boolean(nameError)}
                  aria-describedby={`ssh-key-name-help${nameError ? " ssh-key-name-error" : ""}`}
                />
                <p className="field-help" id="ssh-key-name-help">
                  Required. A recognizable name, such as “Work laptop”. Maximum
                  100 bytes.
                </p>
                {nameError && (
                  <p className="field-error" id="ssh-key-name-error">
                    {nameError}
                  </p>
                )}
                <label htmlFor="ssh-public-key">Public key</label>
                <textarea
                  id="ssh-public-key"
                  required
                  rows={5}
                  maxLength={4096}
                  autoComplete="off"
                  autoCapitalize="none"
                  spellCheck={false}
                  value={publicKey}
                  disabled={busy}
                  onChange={(event) => setPublicKey(event.target.value)}
                  aria-invalid={Boolean(keyError)}
                  aria-describedby={`ssh-public-key-help${keyError ? " ssh-public-key-error" : ""}`}
                />
                <p className="field-help" id="ssh-public-key-help">
                  Required. Paste the contents of a .pub file. Ed25519, RSA
                  (3072–8192 bits), and ECDSA keys are supported. Do not paste a
                  private key, certificate, or multiple keys.
                </p>
                {keyError && (
                  <p className="field-error" id="ssh-public-key-error">
                    {keyError}
                  </p>
                )}
                <div className="form-actions">
                  <button
                    className="button primary"
                    type="submit"
                    disabled={busy}
                  >
                    {busy ? "Adding…" : "Save SSH key"}
                  </button>
                  <button
                    className="button"
                    type="button"
                    disabled={busy}
                    onClick={closeForm}
                  >
                    Cancel
                  </button>
                </div>
              </form>
            </section>
          )}
          <section aria-label="Your SSH keys">
            <div className="section-heading">
              <h2>
                Your keys {keys && <span className="count">{keys.length}</span>}
              </h2>
            </div>
            {listError && (
              <div className="notice error" role="alert">
                {listError}{" "}
                <button
                  className="text-button"
                  type="button"
                  onClick={() => setRefresh((value) => value + 1)}
                >
                  Try again
                </button>
              </div>
            )}
            {!keys && !listError ? (
              <p className="loading" role="status">
                Loading SSH keys…
              </p>
            ) : keys?.length ? (
              <div className="panel token-list">
                {keys.map((key) => (
                  <article
                    className="token-row ssh-key-row"
                    key={key.id}
                    data-testid="ssh-key-row"
                  >
                    <div className="token-row-heading">
                      <h3>{key.name}</h3>
                      <span
                        className={`badge token-status-${key.revokedAt ? "revoked" : "active"}`}
                      >
                        {key.revokedAt ? "Revoked" : "Active"}
                      </span>
                      {!key.revokedAt && (
                        <button
                          className="button danger small"
                          type="button"
                          disabled={Boolean(revoking)}
                          onClick={() => revoke(key)}
                        >
                          {revoking === key.id ? "Revoking…" : "Revoke"}
                        </button>
                      )}
                    </div>
                    <p className="ssh-key-fingerprint">
                      <code>{key.fingerprint}</code>
                    </p>
                    <p className="token-dates">
                      Added {dateLabel(key.createdAt)}
                      {key.revokedAt &&
                        ` · Revoked ${dateLabel(key.revokedAt)}`}
                    </p>
                    <details className="ssh-key-details">
                      <summary>View public key</summary>
                      <pre>{key.publicKey}</pre>
                    </details>
                  </article>
                ))}
              </div>
            ) : (
              !listError && (
                <div className="panel compact-empty">
                  <h3>No SSH keys yet</h3>
                  <p>
                    Add a public key to clone, fetch, and push from your device.
                    HTTPS access tokens remain available.
                  </p>
                </div>
              )
            )}
            <p className="field-help">
              Up to 100 keys, including revoked keys. Revocation is permanent
              for that public key.
            </p>
          </section>
        </div>
      </div>
    </main>
  );
}
