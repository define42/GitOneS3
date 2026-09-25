import { useEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import type { FormEvent } from "react";
import { api, errorMessage } from "./api";
import { SettingsNavigation } from "./SettingsNavigation";
import type {
  AccessToken,
  CreatedAccessToken,
  Repository,
  RepositoryList,
  Session,
  Space,
  TokenPermission,
} from "./api";

function dateLabel(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? value
    : date.toLocaleDateString(undefined, {
        year: "numeric",
        month: "short",
        day: "numeric",
      });
}

function tokenStatus(token: AccessToken): "Active" | "Expired" | "Revoked" {
  if (token.revokedAt) return "Revoked";
  return Date.parse(token.expiresAt) <= Date.now() ? "Expired" : "Active";
}

export function TokensPage({ session }: { session: Session }) {
  const endpoint = `/users/${encodeURIComponent(session.username!)}/tokens`;
  const [tokens, setTokens] = useState<AccessToken[] | null>(null);
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [loadingMore, setLoadingMore] = useState(false);
  const [listing, setListing] = useState(true);
  const [repositories, setRepositories] = useState<Repository[] | null>(null);
  const [listError, setListError] = useState("");
  const [scopeError, setScopeError] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [scopeRefresh, setScopeRefresh] = useState(0);
  const [showForm, setShowForm] = useState(false);
  const [name, setName] = useState("");
  const [permission, setPermission] = useState<TokenPermission>("read");
  const [expiresInDays, setExpiresInDays] = useState(30);
  const [selected, setSelected] = useState<string[]>([]);
  const [allRepositories, setAllRepositories] = useState(false);
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState("all");
  const [busy, setBusy] = useState(false);
  const [revoking, setRevoking] = useState("");
  // The secret lives only in this mounted page. It is never persisted or put in a URL.
  const [created, setCreated] = useState<CreatedAccessToken | null>(null);
  const [copyStatus, setCopyStatus] = useState("");
  const summaryRef = useRef<HTMLDivElement>(null);
  const secretHeadingRef = useRef<HTMLHeadingElement>(null);
  const nameRef = useRef<HTMLInputElement>(null);
  const navigationEpoch = useRef(0);
  const loadedPages = useRef(1);
  const listEpoch = useRef(0);

  useEffect(() => {
    let active = true;
    listEpoch.current += 1;
    setLoadingMore(false);
    setListing(true);
    setListError("");
    async function reloadPages() {
      const records: AccessToken[] = [];
      let after: string | undefined;
      const pageCount = loadedPages.current;
      for (let page = 0; page < pageCount; page += 1) {
        const result = await api<{
          tokens: AccessToken[];
          nextCursor?: string;
        }>(`${endpoint}${after ? `?after=${encodeURIComponent(after)}` : ""}`);
        records.push(...result.tokens);
        after = result.nextCursor;
        if (!after) break;
      }
      return { tokens: records, nextCursor: after };
    }
    reloadPages()
      .then((result) => {
        if (active) {
          setTokens(result.tokens);
          setNextCursor(result.nextCursor);
        }
      })
      .catch((e) => {
        if (active) setListError(errorMessage(e));
      })
      .finally(() => {
        if (active) setListing(false);
      });
    return () => {
      active = false;
      listEpoch.current += 1;
    };
  }, [endpoint, refresh]);

  async function loadMoreTokens() {
    if (!nextCursor || loadingMore || listing) return;
    const epoch = listEpoch.current;
    setLoadingMore(true);
    setListError("");
    try {
      const result = await api<{ tokens: AccessToken[]; nextCursor?: string }>(
        `${endpoint}?after=${encodeURIComponent(nextCursor)}`,
      );
      if (epoch !== listEpoch.current) return;
      setTokens((current) => [
        ...new Map(
          [...(current ?? []), ...result.tokens].map((token) => [
            token.id,
            token,
          ]),
        ).values(),
      ]);
      setNextCursor(result.nextCursor);
      loadedPages.current += 1;
    } catch (e) {
      if (epoch === listEpoch.current) setListError(errorMessage(e));
    } finally {
      if (epoch === listEpoch.current) setLoadingMore(false);
    }
  }

  useEffect(() => {
    if (!showForm || allRepositories) return;
    let active = true;
    const controller = new AbortController();
    setScopeError("");
    async function loadRepositories() {
      const results = await Promise.all(
        Array.from({ length: session.shardCount }, (_, shard) =>
          api<{ spaces: Space[] }>(`/spaces?shard=${shard}`, {
            signal: controller.signal,
          }),
        ),
      );
      const namespaces = [
        ...new Set([
          session.username!,
          ...results
            .flatMap((result) => result.spaces)
            .filter((space) => !space.invited)
            .map((space) => space.name),
        ]),
      ];
      const listings = await Promise.all(
        namespaces.map((namespace) =>
          api<RepositoryList>(`/repos/${encodeURIComponent(namespace)}`, {
            signal: controller.signal,
          }),
        ),
      );
      return listings
        .flatMap((listing) => listing.repositories)
        .sort((a, b) =>
          `${a.namespace}/${a.name}`.localeCompare(`${b.namespace}/${b.name}`),
        );
    }
    loadRepositories()
      .then((result) => {
        if (!active) return;
        setRepositories(result);
        setSelected((current) =>
          current.filter((scope) =>
            result.some((repo) => `${repo.namespace}/${repo.name}` === scope),
          ),
        );
      })
      .catch((e) => {
        if (active) setScopeError(errorMessage(e));
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [
    showForm,
    allRepositories,
    session.shardCount,
    session.username,
    scopeRefresh,
  ]);

  useEffect(() => {
    if (error) summaryRef.current?.focus();
  }, [error]);
  useEffect(() => {
    if (created) secretHeadingRef.current?.focus();
  }, [created]);
  useEffect(() => {
    if (showForm) nameRef.current?.focus();
  }, [showForm]);
  useEffect(() => {
    if (permission === "write" && repositories)
      setSelected((current) =>
        current.filter((scope) =>
          repositories.some(
            (repo) =>
              `${repo.namespace}/${repo.name}` === scope && repo.canWrite,
          ),
        ),
      );
  }, [permission, repositories]);
  useEffect(() => {
    // Clear synchronously before navigation can preserve the page in the back/forward cache.
    const clear = () => {
      navigationEpoch.current += 1;
      flushSync(() => {
        setCreated(null);
        setCopyStatus("");
      });
    };
    window.addEventListener("pagehide", clear);
    return () => {
      navigationEpoch.current += 1;
      window.removeEventListener("pagehide", clear);
    };
  }, []);

  function choosePermission(value: TokenPermission) {
    setPermission(value);
    setStatus("");
    if (value === "write") {
      const writable = selected.filter((scope) =>
        repositories?.some(
          (repo) => `${repo.namespace}/${repo.name}` === scope && repo.canWrite,
        ),
      );
      if (writable.length !== selected.length)
        setStatus("Read-only repositories were removed from your selection.");
      setSelected(writable);
    }
  }

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    setStatus("");
    if (!name.trim() || (!allRepositories && !selected.length)) {
      setError(
        !name.trim()
          ? "Enter a name for this token."
          : "Select at least one repository for this token.",
      );
      summaryRef.current?.focus();
      return;
    }
    setBusy(true);
    const creationEpoch = navigationEpoch.current;
    try {
      const result = await api<CreatedAccessToken>(endpoint, {
        method: "POST",
        csrf: session.csrfToken,
        body: {
          name: name.trim(),
          permission,
          repositories: allRepositories ? [] : selected,
          ...(allRepositories ? { allRepositories: true } : {}),
          expiresInDays,
        },
      });
      setTokens((current) => [result.metadata, ...(current ?? [])]);
      if (creationEpoch === navigationEpoch.current) setCreated(result);
      else
        setStatus(
          "The token was generated but hidden because you left this page. Revoke it and generate a replacement if you did not save it.",
        );
      setCopyStatus("");
      setShowForm(false);
      setName("");
      setSelected([]);
      setAllRepositories(false);
      setPermission("read");
      setExpiresInDays(30);
      setSearch("");
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function copySecret() {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created.token);
      setCopyStatus(
        "Token copied. Store it securely, then close this message.",
      );
    } catch {
      setCopyStatus(
        "Clipboard access was denied. Select and copy the token manually before closing.",
      );
    }
  }

  async function revoke(token: AccessToken) {
    if (
      !window.confirm(
        `Revoke “${token.name}”? Git clients using this token will lose access immediately. This cannot be undone.`,
      )
    )
      return;
    setRevoking(token.id);
    setListError("");
    setStatus("");
    try {
      await api(`${endpoint}/${encodeURIComponent(token.id)}`, {
        method: "DELETE",
        csrf: session.csrfToken,
      });
      setTokens(
        (current) =>
          current?.map((item) =>
            item.id === token.id
              ? { ...item, revokedAt: new Date().toISOString() }
              : item,
          ) ?? null,
      );
      setCreated((current) =>
        current?.metadata.id === token.id ? null : current,
      );
      setStatus(`“${token.name}” was revoked.`);
      setRefresh((value) => value + 1);
    } catch (e) {
      setListError(errorMessage(e));
    } finally {
      setRevoking("");
    }
  }

  const visibleRepositories =
    repositories?.filter((repo) =>
      `${repo.namespace}/${repo.name}`.includes(search.toLowerCase()),
    ) ?? [];
  const visibleTokens =
    tokens?.filter(
      (token) =>
        filter === "all" || tokenStatus(token).toLowerCase() === filter,
    ) ?? [];
  return (
    <main id="main" className="container token-page">
      <a className="back-link" href="/">
        ← Your spaces
      </a>
      <div className="page-heading">
        <div>
          <p className="eyebrow">PERSONAL SETTINGS</p>
          <h1>Access tokens</h1>
          <p className="muted">
            Authenticate Git clients without sharing your sign-in credentials.
          </p>
        </div>
        {!showForm && !created && (
          <button
            className="button primary"
            onClick={() => {
              setShowForm(true);
              setAllRepositories(false);
              setError("");
              setStatus("");
            }}
          >
            Generate new token
          </button>
        )}
      </div>
      <div className="settings-layout">
        <aside className="settings-aside">
          <SettingsNavigation active="tokens" />
          <div className="role-guide">
            <h2>Keep access narrow</h2>
            <p>
              Selected repositories keep a token’s access narrow. All-repository
              tokens also include future repositories your account can access.
              Neither option bypasses your current personal or group role.
            </p>
            <p>Use a separate token for each device or integration.</p>
          </div>
        </aside>
        <div className="settings-content token-content">
          {status && (
            <div className="notice success" role="status">
              {status}
            </div>
          )}
          {created && (
            <section
              className="panel token-reveal"
              aria-label="New access token"
            >
              <h2 ref={secretHeadingRef} tabIndex={-1}>
                Copy your token now
              </h2>
              <p>
                You won’t be able to see it again. Store it in a password
                manager or your Git credential manager.
              </p>
              <p className="token-reveal-scope">
                <strong>
                  {created.metadata.allRepositories
                    ? "All repositories I have access to"
                    : `${created.metadata.repositories.length} selected ${created.metadata.repositories.length === 1 ? "repository" : "repositories"}`}
                </strong>
                {created.metadata.allRepositories &&
                  " — includes current and future personal and group repositories, subject to your current access."}
              </p>
              <label htmlFor="new-access-token">Your new access token</label>
              <div className="copy-field">
                <input
                  id="new-access-token"
                  type="text"
                  readOnly
                  value={created.token}
                  autoComplete="off"
                  spellCheck={false}
                  autoCapitalize="none"
                />
                <button className="button" type="button" onClick={copySecret}>
                  Copy token
                </button>
              </div>
              {copyStatus && (
                <p className="field-help" role="status">
                  {copyStatus}
                </p>
              )}
              <p className="field-help">
                Use <strong>{session.username}</strong> as your Git username and
                this token as the password. Never put the token in a clone URL
                or commit it to a repository.
              </p>
              <button
                className="button primary"
                type="button"
                onClick={() => {
                  setCreated(null);
                  setCopyStatus("");
                  setStatus(
                    "The token has been hidden. If you did not save it, revoke it and generate a replacement.",
                  );
                }}
              >
                I’ve saved it — close
              </button>
            </section>
          )}
          {showForm && (
            <section aria-label="Generate access token">
              <h2>Generate a new token</h2>
              <form className="panel form-panel token-form" onSubmit={create}>
                {error && (
                  <div
                    className="notice error"
                    ref={summaryRef}
                    tabIndex={-1}
                    role="alert"
                  >
                    <h3>Unable to generate token</h3>
                    <p>{error}</p>
                    <a
                      href={
                        !name.trim() ? "#token-name" : "#token-repositories"
                      }
                    >
                      {!name.trim()
                        ? "Review token name"
                        : "Review repository access"}
                    </a>
                  </div>
                )}
                <label htmlFor="token-name">Token name</label>
                <input
                  ref={nameRef}
                  id="token-name"
                  required
                  maxLength={100}
                  autoComplete="off"
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  aria-describedby="token-name-help"
                />
                <p className="field-help" id="token-name-help">
                  A recognizable name, such as “Work laptop” or “Build server”.
                  Maximum 100 characters.
                </p>
                <div className="token-options">
                  <div>
                    <label htmlFor="token-permission">Permission</label>
                    <select
                      id="token-permission"
                      value={permission}
                      onChange={(event) =>
                        choosePermission(event.target.value as TokenPermission)
                      }
                    >
                      <option value="read">Read-only — clone and pull</option>
                      <option value="write">
                        Read and write — clone, pull, and push
                      </option>
                    </select>
                  </div>
                  <div>
                    <label htmlFor="token-expiration">Expiration</label>
                    <select
                      id="token-expiration"
                      value={expiresInDays}
                      onChange={(event) =>
                        setExpiresInDays(Number(event.target.value))
                      }
                    >
                      <option value={7}>7 days</option>
                      <option value={30}>30 days</option>
                      <option value={90}>90 days</option>
                    </select>
                  </div>
                </div>
                <p className="field-help">
                  Write permission includes read access. A token never grants
                  more access than your current personal or group role.
                </p>
                <fieldset
                  id="token-repositories"
                  className="token-scope-fieldset"
                  tabIndex={-1}
                >
                  <legend>Repository access</legend>
                  <div className="token-scope-modes">
                    <label className="token-scope-mode">
                      <input
                        type="radio"
                        name="token-scope-mode"
                        value="selected"
                        checked={!allRepositories}
                        disabled={busy}
                        aria-labelledby="token-scope-selected-label"
                        aria-describedby="token-scope-selected-help"
                        onChange={() => {
                          setAllRepositories(false);
                          setError("");
                        }}
                      />
                      <span>
                        <strong id="token-scope-selected-label">
                          Selected repositories
                        </strong>
                        <span id="token-scope-selected-help" className="muted">
                          Recommended. Only the exact repositories you select,
                          not other or future repositories.
                        </span>
                      </span>
                    </label>
                    <label className="token-scope-mode">
                      <input
                        type="radio"
                        name="token-scope-mode"
                        value="all"
                        checked={allRepositories}
                        disabled={busy}
                        aria-labelledby="token-scope-all-label"
                        aria-describedby="token-scope-all-help"
                        onChange={() => {
                          setAllRepositories(true);
                          setError("");
                          setStatus("");
                        }}
                      />
                      <span>
                        <strong id="token-scope-all-label">
                          All repositories I have access to
                        </strong>
                        <span id="token-scope-all-help" className="muted">
                          Includes current and future repositories in your
                          personal space and groups, whenever you have access.
                        </span>
                      </span>
                    </label>
                  </div>
                  {allRepositories ? (
                    <div className="token-all-warning" role="note">
                      <strong>
                        Broader access, including future repositories.
                      </strong>
                      <p>
                        This token automatically covers new repositories and
                        groups you join. Losing access also removes the token’s
                        access. Write permission still requires your current
                        role to allow writes; it does not grant access to other
                        users’ private repositories.
                      </p>
                    </div>
                  ) : scopeError ? (
                    <div className="notice error" role="alert">
                      {scopeError}{" "}
                      <button
                        className="text-button"
                        type="button"
                        onClick={() => setScopeRefresh((value) => value + 1)}
                      >
                        Retry loading repositories
                      </button>
                    </div>
                  ) : !repositories ? (
                    <p className="loading" role="status">
                      Loading repositories…
                    </p>
                  ) : repositories.length ? (
                    <>
                      <label
                        className="sr-only"
                        htmlFor="token-repository-search"
                      >
                        Find a repository
                      </label>
                      <input
                        id="token-repository-search"
                        placeholder="Find a repository…"
                        value={search}
                        onChange={(event) => setSearch(event.target.value)}
                      />
                      <div className="token-scope-list">
                        {visibleRepositories.length ? (
                          visibleRepositories.map((repo, index) => {
                            const scope = `${repo.namespace}/${repo.name}`;
                            const selectedScope = selected.includes(scope);
                            const readOnly =
                              permission === "write" && !repo.canWrite;
                            return (
                              <label
                                className={`token-scope-row ${readOnly ? "unavailable" : ""}`}
                                key={scope}
                              >
                                <input
                                  type="checkbox"
                                  checked={selectedScope}
                                  disabled={
                                    readOnly ||
                                    (!selectedScope && selected.length >= 100)
                                  }
                                  aria-label={scope}
                                  aria-describedby={`token-scope-description-${index}`}
                                  onChange={(event) =>
                                    setSelected((current) =>
                                      event.target.checked
                                        ? [...current, scope]
                                        : current.filter(
                                            (item) => item !== scope,
                                          ),
                                    )
                                  }
                                />
                                <span>
                                  <strong>{scope}</strong>
                                  <span
                                    className="muted"
                                    id={`token-scope-description-${index}`}
                                  >
                                    {repo.canWrite
                                      ? "Read and write access"
                                      : "Read-only access"}
                                    {readOnly
                                      ? " — unavailable for a write token"
                                      : ""}
                                  </span>
                                </span>
                              </label>
                            );
                          })
                        ) : (
                          <p className="compact-empty">
                            No matching repositories.
                          </p>
                        )}
                      </div>
                      <p className="field-help" aria-live="polite">
                        {selected.length} of 100 repositories selected.
                      </p>
                    </>
                  ) : (
                    <p className="panel compact-empty">
                      No repositories are available.{" "}
                      <a href="/auth/new-repository">Create a repository</a> or
                      join a shared group first.
                    </p>
                  )}
                </fieldset>
                <div className="form-actions">
                  <button
                    className="button primary"
                    type="submit"
                    disabled={
                      busy ||
                      (!allRepositories &&
                        (Boolean(scopeError) || !repositories?.length))
                    }
                  >
                    {busy ? "Generating…" : "Generate token"}
                  </button>
                  <button
                    className="button"
                    type="button"
                    disabled={busy}
                    onClick={() => {
                      setShowForm(false);
                      setError("");
                      setSelected([]);
                      setAllRepositories(false);
                    }}
                  >
                    Cancel
                  </button>
                </div>
              </form>
            </section>
          )}
          <section aria-label="Your access tokens">
            <div className="section-heading">
              <h2>
                Your tokens{" "}
                {tokens && <span className="count">{tokens.length}</span>}
              </h2>
              <div className="token-filter">
                <label htmlFor="token-status-filter">Show</label>
                <select
                  id="token-status-filter"
                  value={filter}
                  onChange={(event) => setFilter(event.target.value)}
                >
                  <option value="all">All tokens</option>
                  <option value="active">Active</option>
                  <option value="expired">Expired</option>
                  <option value="revoked">Revoked</option>
                </select>
              </div>
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
            {!tokens && !listError ? (
              <p className="loading" role="status">
                Loading tokens…
              </p>
            ) : visibleTokens.length ? (
              <div className="panel token-list">
                {visibleTokens.map((token) => (
                  <article
                    className="token-row"
                    key={token.id}
                    data-testid="token-row"
                  >
                    <div className="token-row-heading">
                      <h3>{token.name}</h3>
                      <span
                        className={`badge token-status-${tokenStatus(token).toLowerCase()}`}
                      >
                        {tokenStatus(token)}
                      </span>
                      {!token.revokedAt && (
                        <button
                          className="button danger small"
                          type="button"
                          disabled={Boolean(revoking)}
                          onClick={() => revoke(token)}
                        >
                          {revoking === token.id ? "Revoking…" : "Revoke"}
                        </button>
                      )}
                    </div>
                    <p className="token-dates">
                      <strong>
                        {token.permission === "write"
                          ? "Read and write"
                          : "Read-only"}
                      </strong>{" "}
                      · Created {dateLabel(token.createdAt)} ·{" "}
                      {token.revokedAt
                        ? `Revoked ${dateLabel(token.revokedAt)}`
                        : `Expires ${dateLabel(token.expiresAt)}`}
                    </p>
                    <details className="token-scope-details">
                      <summary>
                        {token.allRepositories
                          ? "All repositories I have access to"
                          : `${token.repositories.length} selected ${token.repositories.length === 1 ? "repository" : "repositories"}`}
                      </summary>
                      {token.allRepositories ? (
                        <p className="muted">
                          Current and future repositories in your personal space
                          and groups, only while your account has access. Write
                          permission never overrides your current role.
                        </p>
                      ) : (
                        <ul>
                          {token.repositories.map((scope) => (
                            <li key={scope}>
                              <code>{scope}</code>
                            </li>
                          ))}
                        </ul>
                      )}
                    </details>
                  </article>
                ))}
              </div>
            ) : (
              !listError && (
                <div className="panel empty-state">
                  <h3>
                    {tokens?.length
                      ? "No matching tokens"
                      : "No access tokens yet"}
                  </h3>
                  <p>
                    {tokens?.length
                      ? "Choose another filter to see your tokens."
                      : "Generate an access token when you are ready to connect a Git client."}
                  </p>
                </div>
              )
            )}
            {nextCursor && (
              <div className="token-load-more">
                <p className="field-help">
                  Filters currently include {tokens?.length ?? 0} loaded tokens.
                  Load more to include additional tokens.
                </p>
                <button
                  className="button"
                  type="button"
                  disabled={loadingMore || listing}
                  onClick={loadMoreTokens}
                >
                  {loadingMore ? "Loading more…" : "Load more tokens"}
                </button>
              </div>
            )}
          </section>
          <div className="info-note">
            <p>
              <strong>For Git over HTTPS.</strong> Enter your personal username{" "}
              <code>{session.username}</code> and use a token in place of a
              password. Tokens do not sign you into the browser or replace your
              identity-provider login.
            </p>
          </div>
        </div>
      </div>
    </main>
  );
}
