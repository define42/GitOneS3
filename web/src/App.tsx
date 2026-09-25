import { useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api, errorMessage, safeReturnTo, validName } from "./api";
import type { Group, Role, Session, Space } from "./api";
import { NewRepository, RepositoryList, RepositoryPage } from "./Repositories";
import { TokensPage } from "./Tokens";

function Icon({
  name = "branch",
  size = 20,
}: {
  name?: "branch" | "group" | "lock" | "plus" | "arrow" | "check";
  size?: number;
}) {
  const paths = {
    branch: (
      <>
        <circle cx="6" cy="5" r="3" />
        <circle cx="6" cy="19" r="3" />
        <circle cx="18" cy="5" r="3" />
        <path d="M6 8v8m12-8a8 8 0 0 1-8 8H6" />
      </>
    ),
    group: (
      <>
        <circle cx="9" cy="7" r="3" />
        <path d="M3 21v-3a6 6 0 0 1 12 0v3m3-17a3 3 0 0 1 0 6m1 4a5 5 0 0 1 3 4v3" />
      </>
    ),
    lock: (
      <>
        <rect x="5" y="10" width="14" height="11" rx="2" />
        <path d="M8 10V6a4 4 0 0 1 8 0v4m-4 5v2" />
      </>
    ),
    plus: <path d="M12 5v14M5 12h14" />,
    arrow: <path d="M5 12h14m-6-6 6 6-6 6" />,
    check: <path d="m5 12 4 4L19 6" />,
  };
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paths[name]}
    </svg>
  );
}

function Notice({
  children,
  success = false,
}: {
  children: ReactNode;
  success?: boolean;
}) {
  return (
    <div
      className={`notice ${success ? "success" : "error"}`}
      role={success ? "status" : "alert"}
    >
      {children}
    </div>
  );
}

function Loading({ text = "Loading your spaces…" }: { text?: string }) {
  return (
    <div className="loading" role="status">
      <span className="spinner" />
      {text}
    </div>
  );
}

function RoleBadge({ role }: { role: Role }) {
  return <span className={`badge role-${role}`}>{role}</span>;
}
function Avatar({ name, large = false }: { name: string; large?: boolean }) {
  return (
    <span className={`avatar ${large ? "large" : ""}`} aria-hidden="true">
      {name.slice(0, 2).toUpperCase()}
    </span>
  );
}

function Header({ session }: { session: Session }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function logout() {
    setBusy(true);
    setError("");
    try {
      await api("/logout", { method: "POST", csrf: session.csrfToken });
      window.location.assign("/auth/login?signedOut=1");
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  }
  return (
    <>
      <a className="skip-link" href="#main">
        Skip to main content
      </a>
      <header className="topbar">
        <a href="/" className="brand">
          <Icon size={28} />
          <span>GitOne</span>
        </a>
        <span className="header-divider" />
        <a href="/" className="header-link">
          Your spaces
        </a>
        <div className="header-actions">
          {session.authenticated ? (
            <>
              <a className="header-link new-link" href="/auth/new-repository">
                <Icon name="plus" size={18} />
                New repository
              </a>
              <a className="header-link new-link" href="/auth/new-group">
                <Icon name="plus" size={18} />
                New group
              </a>
              <a href={`/${session.username}`} className="account-link">
                <Avatar name={session.username!} />
                <span>{session.username}</span>
              </a>
              <a className="header-link" href="/auth/tokens">
                Settings
              </a>
              <button
                className="header-button"
                onClick={logout}
                disabled={busy}
              >
                {busy ? "Signing out…" : "Sign out"}
              </button>
            </>
          ) : (
            <>
              <a className="header-link" href="/auth/login">
                Sign in
              </a>
              <a className="header-button" href="/auth/register">
                Create account
              </a>
            </>
          )}
        </div>
      </header>
      {error && (
        <div className="container">
          <Notice>{error}</Notice>
        </div>
      )}
    </>
  );
}

function Footer() {
  return (
    <footer>
      <span className="footer-brand">
        <Icon size={17} /> GitOne
      </span>
      <span>One place for you and your team.</span>
      <a href="/api/docs">API documentation</a>
    </footer>
  );
}

function Auth({
  session,
  register = false,
}: {
  session: Session;
  register?: boolean;
}) {
  const query = new URLSearchParams(location.search);
  const [username, setUsername] = useState(query.get("username") ?? "");
  const [error, setError] = useState(query.get("error") ?? "");
  const [busy, setBusy] = useState(false);
  const returnTo = safeReturnTo(query.get("returnTo"));
  const provider =
    session.provider === "google" ? "Google" : "your identity provider";
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    if (!validName(username)) {
      setError(
        "Use 1–63 lowercase letters, numbers, or hyphens. Start and end with a letter or number.",
      );
      return;
    }
    setBusy(true);
    try {
      const result = await api<{ available: boolean }>(
        `/names/${encodeURIComponent(username)}`,
      );
      if (register && !result.available)
        throw new Error(
          "This username is already taken. Choose another username or sign in.",
        );
      if (!register && result.available)
        throw new Error(
          "This username has not been registered. Create an account first.",
        );
      const params = new URLSearchParams({
        mode: register ? "register" : "login",
        ui: "1",
        returnTo: returnTo === "/" ? `/${username}` : returnTo,
      });
      window.location.assign(`/${username}/auth/oidc/login?${params}`);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  }
  const alternateQuery =
    returnTo !== "/" ? `?returnTo=${encodeURIComponent(returnTo)}` : "";
  return (
    <main id="main" className="auth-main">
      <div className="auth-mark">
        <Icon size={36} />
      </div>
      <h1>{register ? "Create your GitOne account" : "Sign in to GitOne"}</h1>
      <p className="auth-subtitle">
        {register
          ? "A space of your own. A home for your team."
          : "Welcome back. Your spaces are waiting."}
      </p>
      {query.has("signedOut") && (
        <Notice success>
          Signed out of GitOne. Your identity provider may still be signed in.
        </Notice>
      )}
      <form className="panel auth-card" onSubmit={submit}>
        <label htmlFor="username">Username</label>
        <input
          id="username"
          name="username"
          autoComplete="username"
          autoCapitalize="none"
          spellCheck={false}
          required
          maxLength={63}
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          aria-describedby="username-help auth-error"
          aria-invalid={!!error}
        />
        <p id="username-help" className="field-help">
          {register
            ? "Your personal space will be /"
            : "Use the username of your personal space: /"}
          <strong>{username || "username"}</strong>
        </p>
        {error && (
          <div id="auth-error">
            <Notice>{error}</Notice>
          </div>
        )}
        {register && (
          <p className="registration-note">
            <Icon name="lock" size={17} />
            After verification, this username is permanently linked to your
            identity provider account.
          </p>
        )}
        <button
          className="button primary full-width"
          type="submit"
          disabled={busy}
        >
          {busy ? "Connecting…" : `Continue with ${provider}`}
          <Icon name="arrow" size={17} />
        </button>
        <p className="field-help centered">
          You’ll securely sign in with {provider}.<br />
          GitOne never receives your password.
        </p>
      </form>
      <div className="panel auth-switch">
        {register ? (
          <>
            Already have an account?{" "}
            <a href={`/auth/login${alternateQuery}`}>Sign in</a>
          </>
        ) : (
          <>
            New to GitOne?{" "}
            <a href={`/auth/register${alternateQuery}`}>Create an account</a>
          </>
        )}
      </div>
    </main>
  );
}

function Welcome() {
  return (
    <main id="main" className="welcome container">
      <div className="eyebrow">
        <Icon name="branch" size={17} /> Your work. Your space.
      </div>
      <h1>
        A home for you.
        <br />
        <span>A space for your team.</span>
      </h1>
      <p className="welcome-copy">
        Claim your personal namespace, bring your team together, and manage
        access in one place.
      </p>
      <div className="welcome-actions">
        <a className="button primary" href="/auth/register">
          Create your account
          <Icon name="arrow" size={18} />
        </a>
        <a className="button" href="/auth/login">
          Sign in to GitOne
        </a>
      </div>
      <div className="welcome-grid">
        <div className="panel feature">
          <Icon name="branch" size={27} />
          <h2>Make it yours</h2>
          <p>
            Choose a username and make <code>/your-name</code> your personal
            space.
          </p>
        </div>
        <div className="panel feature">
          <Icon name="group" size={27} />
          <h2>Build a team</h2>
          <p>
            Create a shared group and invite collaborators by their GitOne
            username.
          </p>
        </div>
        <div className="panel feature">
          <Icon name="lock" size={27} />
          <h2>Stay in control</h2>
          <p>Give each member the right access: reader, developer, or owner.</p>
        </div>
      </div>
    </main>
  );
}

function Sidebar({ session }: { session: Session }) {
  return (
    <aside className="sidebar">
      <div className="identity">
        <Avatar name={session.username!} large />
        <strong>{session.username}</strong>
        <span className="muted">Personal account</span>
      </div>
      <nav aria-label="Workspace">
        <a href="/" className="side-link">
          <Icon name="branch" />
          Your spaces
        </a>
        <a href={`/${session.username}`} className="side-link">
          <Icon name="lock" />
          Personal space
        </a>
        <a href="/auth/new-repository" className="side-link">
          <Icon name="plus" />
          New repository
        </a>
        <a href="/auth/new-group" className="side-link">
          <Icon name="plus" />
          Create a group
        </a>
        <a href="/auth/tokens" className="side-link">
          <Icon name="lock" />
          Settings
        </a>
      </nav>
      <div className="sidebar-note">
        <strong>Better together</strong>
        <p>
          Groups give your team a shared namespace, with access you control.
        </p>
      </div>
    </aside>
  );
}

function Dashboard({ session }: { session: Session }) {
  const [spaces, setSpaces] = useState<Space[] | null>(null);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState("");
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    let active = true;
    setError("");
    Promise.all(
      Array.from({ length: session.shardCount }, (_, shard) =>
        api<{ spaces: Space[] }>(`/spaces?shard=${shard}`),
      ),
    )
      .then((results) => {
        if (active)
          setSpaces(
            results
              .flatMap((r) => r.spaces)
              .sort((a, b) => a.name.localeCompare(b.name)),
          );
      })
      .catch((e) => {
        if (active) setError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, [session.shardCount, refresh]);
  const invited = spaces?.filter((s) => s.invited) ?? [];
  const groups =
    spaces?.filter(
      (s) => !s.invited && s.name.includes(filter.toLowerCase()),
    ) ?? [];
  return (
    <main id="main" className="workspace container">
      <Sidebar session={session} />
      <div className="workspace-content">
        <div className="page-heading">
          <div>
            <p className="eyebrow">YOUR WORKSPACE</p>
            <h1>Your spaces</h1>
            <p className="muted">
              A personal home and the teams you’re part of.
            </p>
          </div>
          <a className="button primary" href="/auth/new-group">
            <Icon name="plus" size={17} />
            New group
          </a>
        </div>
        <section aria-label="Personal space" className="panel personal-card">
          <Avatar name={session.username!} />
          <div>
            <a className="space-name" href={`/${session.username}`}>
              /{session.username}
            </a>
            <p className="muted">Your personal namespace</p>
          </div>
          <span className="badge">Personal</span>
          <span className="personal-email">{session.identity?.email}</span>
        </section>
        <RepositoryList namespace={session.username!} />
        {error && (
          <Notice>
            {error}{" "}
            <button
              className="text-button"
              onClick={() => setRefresh((v) => v + 1)}
            >
              Try again
            </button>
          </Notice>
        )}
        {invited.length > 0 && (
          <section className="invitations-section">
            <h2>
              Pending invitations{" "}
              <span className="count">{invited.length}</span>
            </h2>
            {invited.map((space) => (
              <div className="panel invitation-row" key={space.name}>
                <Icon name="group" />
                <div>
                  <strong>You’re invited to /{space.name}</strong>
                  <p className="muted">Join this group as a {space.role}.</p>
                </div>
                <a
                  className="button"
                  href={`/${space.name}/invitations/accept`}
                >
                  Review invitation
                </a>
              </div>
            ))}
          </section>
        )}
        <section>
          <div className="section-heading">
            <h2>
              Shared groups{" "}
              <span className="count">
                {spaces?.filter((s) => !s.invited).length ?? 0}
              </span>
            </h2>
          </div>
          <label htmlFor="group-search" className="sr-only">
            Find a group
          </label>
          <input
            id="group-search"
            className="group-search"
            placeholder="Find a group…"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
          {!spaces && !error ? (
            <Loading />
          ) : groups.length ? (
            <div className="panel group-list">
              {groups.map((space) => (
                <a
                  href={`/${space.name}`}
                  className="group-row"
                  key={space.name}
                >
                  <span className="group-icon">
                    <Icon name="group" />
                  </span>
                  <div>
                    <strong>/{space.name}</strong>
                    <p>Shared group</p>
                  </div>
                  <RoleBadge role={space.role} />
                  <Icon name="arrow" size={18} />
                </a>
              ))}
            </div>
          ) : (
            !error && (
              <div className="panel empty-state">
                <div className="empty-icon">
                  <Icon name="group" size={30} />
                </div>
                <h3>
                  {filter
                    ? "No matching groups"
                    : "Good work starts with a team"}
                </h3>
                <p>
                  {filter
                    ? "Try searching for a different group name."
                    : "Create a shared space, invite your collaborators, and decide who can do what."}
                </p>
                {!filter && (
                  <a className="button" href="/auth/new-group">
                    Create your first group
                  </a>
                )}
              </div>
            )
          )}
        </section>
        <div className="info-note">
          <Icon name="branch" size={19} />
          <p>
            <strong>Your projects, your team.</strong> Create private
            repositories in your personal space or a shared group. Repository
            access follows the space’s membership.
          </p>
        </div>
      </div>
    </main>
  );
}

function NewGroup({ session }: { session: Session }) {
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    if (!validName(name)) {
      setError(
        "Use 1–63 lowercase letters, numbers, or hyphens. Start and end with a letter or number.",
      );
      return;
    }
    setBusy(true);
    try {
      const result = await api<{ available: boolean }>(
        `/names/${encodeURIComponent(name)}`,
      );
      if (!result.available)
        throw new Error(
          "This name is already taken. Users and groups share the same namespace.",
        );
      await api(`/groups/${encodeURIComponent(name)}`, {
        method: "POST",
        csrf: session.csrfToken,
      });
      location.assign(`/${name}`);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  }
  return (
    <main id="main" className="narrow container">
      <a href="/" className="back-link">
        ← Back to your spaces
      </a>
      <div className="page-heading">
        <div>
          <h1>Create a shared group</h1>
          <p className="muted">Give your team a place to work together.</p>
        </div>
      </div>
      <form className="panel form-panel" onSubmit={submit}>
        <label htmlFor="group-name">Group name</label>
        <div className="input-prefix">
          <span>/</span>
          <input
            id="group-name"
            required
            maxLength={63}
            autoCapitalize="none"
            autoComplete="off"
            spellCheck={false}
            value={name}
            onChange={(e) => setName(e.target.value)}
            aria-describedby="group-name-help group-error"
            aria-invalid={!!error}
          />
        </div>
        <p id="group-name-help" className="field-help">
          Lowercase letters, numbers, and hyphens. Must be available across all
          users and groups.
        </p>
        <div className="owner-preview">
          <Avatar name={session.username!} />
          <div>
            <strong>{session.username}</strong>
            <p className="muted">
              You’ll be the first owner. Invite your team after creating the
              group.
            </p>
          </div>
          <RoleBadge role="owner" />
        </div>
        {error && (
          <div id="group-error">
            <Notice>{error}</Notice>
          </div>
        )}
        <div className="form-actions">
          <button className="button primary" type="submit" disabled={busy}>
            {busy ? "Creating…" : "Create group"}
          </button>
          <a className="button" href="/">
            Cancel
          </a>
        </div>
      </form>
    </main>
  );
}

function MemberRow({
  userId,
  role,
  session,
  owner,
  pending = false,
  mutate,
}: {
  userId: string;
  role: Role;
  session: Session;
  owner: boolean;
  pending?: boolean;
  mutate: (method: string, path: string, body: unknown) => Promise<void>;
}) {
  const [nextRole, setNextRole] = useState<Role>(role);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const self = userId === session.userId;
  const label = self ? `${session.username} (you)` : userId;
  async function change(remove: boolean) {
    if (
      remove &&
      !window.confirm(
        pending
          ? "Cancel this invitation?"
          : "Remove this member from the group? They will lose access immediately.",
      )
    )
      return;
    setBusy(true);
    setError("");
    try {
      await mutate(
        remove ? "DELETE" : "PUT",
        pending ? "/invitations" : "/members",
        { userId, ...(remove ? {} : { role: nextRole }) },
      );
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="member-row" data-testid="member-row">
      <div className="member-main">
        <Avatar name={self ? session.username! : "ID"} />
        <div className="member-identity" data-user-id={userId}>
          <strong title={userId}>{label}</strong>
          <span className="muted">
            {pending
              ? "Invitation pending"
              : self
                ? "Signed-in account"
                : "Verified identity"}
          </span>
        </div>
        <RoleBadge role={role} />
      </div>
      {owner && (
        <div className="member-actions">
          {!pending && (
            <>
              <label className="sr-only" htmlFor={`role-${userId}`}>
                Role for {label}
              </label>
              <select
                id={`role-${userId}`}
                value={nextRole}
                onChange={(e) => setNextRole(e.target.value as Role)}
                disabled={busy}
              >
                <option value="reader">Reader</option>
                <option value="developer">Developer</option>
                <option value="owner">Owner</option>
              </select>
              <button
                className="button small"
                disabled={busy || nextRole === role}
                onClick={() => change(false)}
              >
                Update role
              </button>
            </>
          )}
          <button
            className="button small danger"
            disabled={busy}
            onClick={() => change(true)}
          >
            {pending ? "Cancel invitation" : "Remove"}
          </button>
        </div>
      )}
      {error && <Notice>{error}</Notice>}
    </div>
  );
}

function GroupPage({
  session,
  name,
  settings,
}: {
  session: Session;
  name: string;
  settings: boolean;
}) {
  const [group, setGroup] = useState<Group | null>(null);
  const [error, setError] = useState("");
  const [username, setUsername] = useState("");
  const [role, setRole] = useState<Role>("developer");
  const [busy, setBusy] = useState(false);
  const [inviteError, setInviteError] = useState("");
  const [success, setSuccess] = useState("");
  useEffect(() => {
    let active = true;
    api<Group>(`/groups/${encodeURIComponent(name)}`)
      .then((g) => {
        if (active) setGroup(g);
      })
      .catch((e) => {
        if (active) setError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, [name]);
  async function mutate(method: string, path: string, body: unknown) {
    const updated = await api<Group | undefined>(
      `/groups/${encodeURIComponent(name)}${path}`,
      { method, body, csrf: session.csrfToken },
    );
    if (!updated) {
      setGroup(null);
      location.assign("/");
      return;
    }
    setGroup(updated);
  }
  async function invite(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setInviteError("");
    setSuccess("");
    try {
      if (!validName(username))
        throw new Error("Enter a valid GitOne username.");
      const target = await api<{ username: string; userId: string }>(
        `/users/${encodeURIComponent(username)}`,
      );
      await mutate("POST", "/invitations", { userId: target.userId, role });
      setSuccess(
        `Invitation sent to ${username}. They can accept it from their spaces page.`,
      );
      setUsername("");
    } catch (e) {
      setInviteError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }
  if (error)
    return (
      <main id="main" className="narrow container">
        <a className="back-link" href="/">
          ← Back to your spaces
        </a>
        <h1>Unable to open /{name}</h1>
        <Notice>{error}</Notice>
        <p>If you were invited to this group, accept your invitation first.</p>
        <a className="button" href={`/${name}/invitations/accept`}>
          Review invitation
        </a>
      </main>
    );
  if (!group)
    return (
      <main id="main" className="container">
        <Loading text="Loading group…" />
      </main>
    );
  const owner = group.role === "owner";
  return (
    <main id="main" className="container group-page">
      <a className="back-link" href="/">
        ← Your spaces
      </a>
      <div className="group-heading">
        <span className="group-logo">
          <Icon name="group" size={32} />
        </span>
        <div>
          <div className="title-line">
            <h1>{name}</h1>
            <span className="badge">Shared group</span>
          </div>
          <p className="muted">
            A shared namespace for your team · <code>/{name}</code>
          </p>
        </div>
        <RoleBadge role={group.role} />
      </div>
      <nav className="tabs" aria-label="Group">
        <a href={`/${name}`} aria-current={!settings ? "page" : undefined}>
          <Icon name="group" size={17} />
          Overview
          <span className="count">{Object.keys(group.members).length}</span>
        </a>
        {owner && (
          <a
            href={`/${name}/settings`}
            aria-current={settings ? "page" : undefined}
          >
            <Icon name="lock" size={17} />
            Settings & members
          </a>
        )}
      </nav>
      {settings && !owner ? (
        <Notice>Only group owners can manage members and invitations.</Notice>
      ) : settings ? (
        <div className="settings-layout">
          <aside className="settings-aside">
            <strong>Group settings</strong>
            <p>Manage the people who have access to /{name}.</p>
            <div className="role-guide">
              <h2>Group roles</h2>
              <p>
                <strong>Owner</strong> manages members and access.
              </p>
              <p>
                <strong>Developer</strong> can read and write.
              </p>
              <p>
                <strong>Reader</strong> has read-only access.
              </p>
            </div>
          </aside>
          <div className="settings-content">
            <section>
              <h2>Invite a collaborator</h2>
              <p className="muted">
                Invite an existing GitOne user. Access begins when they accept.
              </p>
              <form className="panel form-panel" onSubmit={invite}>
                <div className="invite-fields">
                  <div>
                    <label htmlFor="invite-username">Invite username</label>
                    <input
                      id="invite-username"
                      value={username}
                      onChange={(e) => setUsername(e.target.value)}
                      required
                      autoCapitalize="none"
                      autoComplete="off"
                      spellCheck={false}
                      aria-describedby="invite-help invite-error"
                    />
                  </div>
                  <div>
                    <label htmlFor="invitation-role">Invitation role</label>
                    <select
                      id="invitation-role"
                      value={role}
                      onChange={(e) => setRole(e.target.value as Role)}
                    >
                      <option value="reader">Reader</option>
                      <option value="developer">Developer</option>
                      <option value="owner">Owner</option>
                    </select>
                  </div>
                </div>
                <p id="invite-help" className="field-help">
                  They must register a personal username before you can invite
                  them.
                </p>
                {inviteError && (
                  <div id="invite-error">
                    <Notice>{inviteError}</Notice>
                  </div>
                )}
                {success && <Notice success>{success}</Notice>}
                <button
                  className="button primary"
                  disabled={busy}
                  type="submit"
                >
                  {busy ? "Sending…" : "Send invitation"}
                </button>
              </form>
            </section>
            <section>
              <h2>
                Members{" "}
                <span className="count">
                  {Object.keys(group.members).length}
                </span>
              </h2>
              <p className="muted">
                A group must always have at least one owner.
              </p>
              <div className="panel member-list">
                {Object.entries(group.members).map(([id, memberRole]) => (
                  <MemberRow
                    key={`${id}-${memberRole}`}
                    userId={id}
                    role={memberRole}
                    session={session}
                    owner
                    mutate={mutate}
                  />
                ))}
              </div>
            </section>
            <section>
              <h2>
                Pending invitations{" "}
                <span className="count">
                  {Object.keys(group.invitations ?? {}).length}
                </span>
              </h2>
              {Object.keys(group.invitations ?? {}).length ? (
                <div className="panel member-list">
                  {Object.entries(group.invitations ?? {}).map(
                    ([id, invitationRole]) => (
                      <MemberRow
                        key={id}
                        userId={id}
                        role={invitationRole}
                        session={session}
                        owner
                        pending
                        mutate={mutate}
                      />
                    ),
                  )}
                </div>
              ) : (
                <p className="panel compact-empty">No pending invitations.</p>
              )}
            </section>
          </div>
        </div>
      ) : (
        <div className="group-overview">
          <div>
            <RepositoryList namespace={name} />
          </div>
          <aside className="group-about">
            <h2>About this group</h2>
            <p>
              <Icon name="group" size={17} />{" "}
              {Object.keys(group.members).length} member
              {Object.keys(group.members).length !== 1 ? "s" : ""}
            </p>
            <p>
              <Icon name="lock" size={17} /> Membership required
            </p>
            <hr />
            <h2>Your access</h2>
            <RoleBadge role={group.role} />
            <p className="muted">
              {owner
                ? "You can manage members and invitations."
                : "Contact a group owner to change your access."}
            </p>
          </aside>
        </div>
      )}
    </main>
  );
}

function Invitation({ session, name }: { session: Session; name: string }) {
  const [invitation, setInvitation] = useState<{
    name: string;
    role: Role;
  } | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    api<{ name: string; role: Role }>(
      `/groups/${encodeURIComponent(name)}/invitation`,
    )
      .then((data) => {
        if (active) setInvitation(data);
      })
      .catch((e) => {
        if (active) setError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, [name]);
  async function accept() {
    setBusy(true);
    setError("");
    try {
      await api(`/groups/${encodeURIComponent(name)}/invitations/accept`, {
        method: "POST",
        csrf: session.csrfToken,
      });
      location.assign(`/${name}`);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  }
  return (
    <main id="main" className="narrow container">
      <a href="/" className="back-link">
        ← Your spaces
      </a>
      <section className="panel invitation-card">
        <div className="empty-icon">
          <Icon name="group" size={34} />
        </div>
        <h1>You’re invited to /{name}</h1>
        {error && <Notice>{error}</Notice>}
        {!invitation && !error ? (
          <Loading text="Loading invitation…" />
        ) : (
          invitation && (
            <>
              <p>
                Join this shared group as a <strong>{invitation.role}</strong>.
              </p>
              <p className="muted">
                You’re accepting as <strong>{session.username}</strong>. The
                group owner can change or revoke your access.
              </p>
              <button
                className="button primary"
                onClick={accept}
                disabled={busy}
              >
                {busy ? "Joining…" : "Accept invitation"}
              </button>
            </>
          )
        )}
        <a className="text-link" href="/">
          Back to your spaces
        </a>
      </section>
    </main>
  );
}

export function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    api<Session>("/session")
      .then((s) => {
        if (active) setSession(s);
      })
      .catch((e) => {
        if (active) setError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, []);
  const path = location.pathname.replace(/\/$/, "") || "/";
  const destination = path + location.search;
  const publicPage =
    path === "/" || path === "/auth/login" || path === "/auth/register";
  useEffect(() => {
    if (session && !session.authenticated && !publicPage)
      location.replace(
        `/auth/login?returnTo=${encodeURIComponent(safeReturnTo(destination))}`,
      );
  }, [session, publicPage, destination]);
  if (error)
    return (
      <main id="main" className="narrow container">
        <h1>GitOne is unavailable</h1>
        <Notice>{error}</Notice>
        <p>
          Authentication or the API may not be configured. Check the server
          configuration, then try again.
        </p>
        <button className="button" onClick={() => location.reload()}>
          Try again
        </button>
      </main>
    );
  if (!session) return <Loading text="Connecting to GitOne…" />;
  let content: ReactNode;
  if (path === "/auth/login" || path === "/auth/register")
    content = <Auth session={session} register={path === "/auth/register"} />;
  else if (!session.authenticated)
    content = publicPage ? (
      <Welcome />
    ) : (
      <Loading text="Redirecting to sign in…" />
    );
  else if (path === "/" || path === `/${session.username}`)
    content = <Dashboard session={session} />;
  else if (path === "/auth/new-group") content = <NewGroup session={session} />;
  else if (path === "/auth/new-repository")
    content = <NewRepository session={session} />;
  else if (path === "/auth/tokens") content = <TokensPage session={session} />;
  else {
    const [, name, subpath, action] = path.split("/");
    content =
      subpath === "invitations" && action === "accept" ? (
        <Invitation session={session} name={name} />
      ) : subpath && subpath !== "settings" ? (
        <RepositoryPage session={session} namespace={name} name={subpath} />
      ) : (
        <GroupPage
          session={session}
          name={name}
          settings={subpath === "settings"}
        />
      );
  }
  return (
    <>
      <Header session={session} />
      {content}
      <Footer />
    </>
  );
}
