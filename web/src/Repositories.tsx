import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { api, errorMessage, validRepositoryName } from "./api";
import { loadSpaces } from "./spaces";
import type {
  Repository,
  RepositoryBlob,
  RepositoryBranch,
  RepositoryCommit,
  RepositoryList as RepositoryListData,
  RepositoryTree,
  Session,
  Space,
} from "./api";

function RepoIcon({ directory = false }: { directory?: boolean }) {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {directory ? (
        <path d="M3 7V5h7l2 3h9v12H3V7Z" />
      ) : (
        <>
          <path d="M6 3h9l4 4v14H6V3Zm9 0v5h4" />
          <path d="M9 12h7m-7 4h7" />
        </>
      )}
    </svg>
  );
}

function ErrorNotice({ message }: { message: string }) {
  return (
    <div className="notice error" role="alert">
      {message}
    </div>
  );
}

function Loading({ text = "Loading repositories…" }: { text?: string }) {
  return (
    <div className="loading" role="status">
      <span className="spinner" />
      {text}
    </div>
  );
}

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

function byteLabel(value: number) {
  return value < 1024 ? `${value} bytes` : `${(value / 1024).toFixed(1)} KB`;
}

export function RepositoryList({ namespace }: { namespace: string }) {
  const [data, setData] = useState<RepositoryListData | null>(null);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState("");
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    let active = true;
    setError("");
    api<RepositoryListData>(`/repos/${encodeURIComponent(namespace)}`)
      .then((result) => {
        if (active) setData(result);
      })
      .catch((e) => {
        if (active) setError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, [namespace, refresh]);
  const repositories =
    data?.repositories.filter((repo) =>
      `${repo.name} ${repo.description}`
        .toLowerCase()
        .includes(filter.toLowerCase()),
    ) ?? [];
  return (
    <section
      className="repository-section"
      aria-label={`Repositories in ${namespace}`}
    >
      <div className="section-heading">
        <h2>
          Repositories{" "}
          {data && <span className="count">{data.repositories.length}</span>}
        </h2>
        {data?.canWrite && (
          <a
            className="button primary"
            href={`/auth/new-repository?namespace=${encodeURIComponent(namespace)}`}
          >
            New repository
          </a>
        )}
      </div>
      {error ? (
        <>
          <ErrorNotice message={error} />
          <button
            className="button"
            onClick={() => setRefresh((value) => value + 1)}
          >
            Try again
          </button>
        </>
      ) : !data ? (
        <Loading />
      ) : (
        <>
          {data.repositories.length > 0 && (
            <>
              <label className="sr-only" htmlFor={`repo-search-${namespace}`}>
                Find a repository
              </label>
              <input
                id={`repo-search-${namespace}`}
                className="group-search"
                placeholder="Find a repository…"
                value={filter}
                onChange={(event) => setFilter(event.target.value)}
              />
            </>
          )}
          {repositories.length ? (
            <div className="panel repository-list">
              {repositories.map((repo) => (
                <article className="repository-row" key={repo.id}>
                  <div className="title-line">
                    <a
                      className="repository-name"
                      href={`/${namespace}/${repo.name}`}
                    >
                      <RepoIcon />
                      {repo.name}
                    </a>
                    <span className="badge">Private</span>
                  </div>
                  {repo.description && <p>{repo.description}</p>}
                  <div className="repository-meta">
                    <span>
                      {repo.empty ? "Empty repository" : repo.defaultBranch}
                    </span>
                    <span>Created {dateLabel(repo.createdAt)}</span>
                  </div>
                </article>
              ))}
            </div>
          ) : (
            <div className="panel empty-state">
              <div className="empty-icon">
                <RepoIcon />
              </div>
              <h3>
                {filter
                  ? "No matching repositories"
                  : "A home for your next project"}
              </h3>
              <p>
                {filter
                  ? "Try a different name or description."
                  : data.canWrite
                    ? "Create a private repository, add a README, and start exploring your code."
                    : "This space has no repositories yet. An owner or developer can create one."}
              </p>
            </div>
          )}
          {!data.canWrite && (
            <p className="field-help">
              You have read-only access to repositories in this space.
            </p>
          )}
        </>
      )}
    </section>
  );
}

export function NewRepository({ session }: { session: Session }) {
  const requestedOwner = new URLSearchParams(location.search).get("namespace");
  const [namespace, setNamespace] = useState(
    requestedOwner ?? session.username!,
  );
  const [groups, setGroups] = useState<Space[] | null>(null);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [readme, setReadme] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [ownerError, setOwnerError] = useState("");
  const errorSummary = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (error) errorSummary.current?.focus();
  }, [error]);
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    setGroups(null);
    setOwnerError("");
    loadSpaces(session.shardCount, controller.signal)
      .then((spaces) => {
        if (!active) return;
        const writable = spaces.filter(
          (space) => !space.invited && space.role !== "reader",
        );
        setGroups(writable);
        if (
          requestedOwner &&
          requestedOwner !== session.username &&
          !writable.some((space) => space.name === requestedOwner)
        ) {
          setOwnerError(
            "You cannot create repositories in that space. Choose your personal space or a group where you are an owner or developer.",
          );
          setNamespace(session.username!);
        }
      })
      .catch((e) => {
        if (active) setOwnerError(errorMessage(e));
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [requestedOwner, session.shardCount, session.username]);
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    if (!validRepositoryName(name)) {
      setError(
        "Use 1–63 lowercase letters, numbers, dots, underscores, or hyphens. Start and end with a letter or number. Do not use consecutive dots, a .git suffix, or a reserved name (auth, settings, members, invitations).",
      );
      return;
    }
    setBusy(true);
    try {
      const repo = await api<Repository>(
        `/repos/${encodeURIComponent(namespace)}`,
        {
          method: "POST",
          csrf: session.csrfToken,
          body: {
            name,
            description,
            defaultBranch: "main",
            initializeReadme: readme,
          },
        },
      );
      location.assign(`/${repo.namespace}/${repo.name}`);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  }
  return (
    <main id="main" className="narrow container">
      <a
        className="back-link"
        href={requestedOwner ? `/${encodeURIComponent(requestedOwner)}` : "/"}
      >
        ← Back to your space
      </a>
      <div className="page-heading">
        <div>
          <h1>Create a new repository</h1>
          <p className="muted">
            A private home for your project’s files and history.
          </p>
        </div>
      </div>
      <form className="panel form-panel" onSubmit={submit}>
        {ownerError && <ErrorNotice message={ownerError} />}
        {!groups ? (
          ownerError ? (
            <button
              className="button"
              type="button"
              onClick={() => location.reload()}
            >
              Try again
            </button>
          ) : (
            <Loading text="Loading available owners…" />
          )
        ) : (
          <>
            <div className="repository-name-fields">
              <div>
                <label htmlFor="repository-owner">Owner</label>
                <select
                  id="repository-owner"
                  value={namespace}
                  onChange={(event) => {
                    setNamespace(event.target.value);
                    setOwnerError("");
                  }}
                >
                  <option value={session.username}>
                    {session.username} (personal)
                  </option>
                  {groups.map((space) => (
                    <option key={space.name} value={space.name}>
                      {space.name} (group)
                    </option>
                  ))}
                </select>
              </div>
              <span className="repository-slash" aria-hidden="true">
                /
              </span>
              <div>
                <label htmlFor="repository-name">Repository name</label>
                <input
                  id="repository-name"
                  required
                  maxLength={63}
                  autoComplete="off"
                  autoCapitalize="none"
                  spellCheck={false}
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  aria-describedby={`repository-name-help${error ? " repository-error" : ""}`}
                  aria-invalid={Boolean(error)}
                />
              </div>
            </div>
            <p id="repository-name-help" className="field-help">
              Lowercase letters, numbers, dots, underscores, and hyphens. The
              name must be available within the owner’s space.
            </p>
            <label htmlFor="repository-description">Description</label>
            <textarea
              id="repository-description"
              rows={3}
              maxLength={500}
              value={description}
              onChange={(event) => setDescription(event.target.value)}
            />
            <p className="field-help">Optional. Up to 500 characters.</p>
            <div className="repository-privacy">
              <span className="badge">Private</span>
              <div>
                <strong>Access follows the owner’s space</strong>
                <p className="muted">
                  {namespace === session.username
                    ? "Only you can access this personal repository."
                    : "Group readers can browse. Developers and owners can create repositories. Membership changes apply immediately."}
                </p>
              </div>
            </div>
            <label className="checkbox-field" htmlFor="repository-readme">
              <input
                id="repository-readme"
                type="checkbox"
                checked={readme}
                onChange={(event) => setReadme(event.target.checked)}
                aria-labelledby="repository-readme-label"
                aria-describedby="repository-readme-help"
              />
              <span>
                <strong id="repository-readme-label">Add a README</strong>
                <span className="field-help" id="repository-readme-help">
                  Create the first commit on main with your project name and
                  description.
                </span>
              </span>
            </label>
            <p className="field-help">
              Clone and push over HTTPS using a personal access token
              {session.sshURL
                ? ", or over SSH using a registered public key."
                : "."}
            </p>
            {error && (
              <div id="repository-error" ref={errorSummary} tabIndex={-1}>
                <ErrorNotice message={error} />
                <a href="#repository-name">Review the repository name</a>
              </div>
            )}
            <div className="form-actions">
              <button className="button primary" type="submit" disabled={busy}>
                {busy ? "Creating…" : "Create repository"}
              </button>
              <a className="button" href={`/${namespace}`}>
                Cancel
              </a>
            </div>
          </>
        )}
      </form>
    </main>
  );
}

type BrowserData = {
  tree?: RepositoryTree;
  blob?: RepositoryBlob;
  readme?: RepositoryBlob;
  commits?: RepositoryCommit[];
};

function shellQuote(value: string) {
  return "'" + value.replace(/'/g, "'\\''") + "'";
}

type CloneProtocol = "HTTPS" | "SSH";

function sshCloneURL(repository: Repository, session: Session): string {
  if (!session.sshURL || !session.username) return "";
  try {
    const url = new URL(session.sshURL);
    if (url.protocol !== "ssh:") return "";
    url.username = session.username;
    url.password = "";
    url.pathname = `/${repository.namespace}/${repository.name}.git`;
    url.search = "";
    url.hash = "";
    return url.toString();
  } catch {
    return "";
  }
}

function repositoryCloneURL(
  repository: Repository,
  session: Session,
  protocol: CloneProtocol,
) {
  return (
    (protocol === "SSH" && sshCloneURL(repository, session)) ||
    `${location.origin}/${repository.namespace}/${repository.name}.git`
  );
}

function ClonePanel({
  repository,
  session,
  protocol,
  onProtocolChange,
  expanded = false,
}: {
  repository: Repository;
  session: Session;
  protocol: CloneProtocol;
  onProtocolChange: (value: CloneProtocol) => void;
  expanded?: boolean;
}) {
  const cloneURL = repositoryCloneURL(repository, session, protocol);
  const [status, setStatus] = useState("");
  async function copyURL() {
    try {
      await navigator.clipboard.writeText(cloneURL);
      setStatus("Clone URL copied.");
    } catch {
      setStatus(
        "Clipboard access was denied. Select and copy the URL manually.",
      );
    }
  }
  return (
    <details className="panel clone-panel" open={expanded || undefined}>
      <summary>Clone with {protocol}</summary>
      <div className="clone-content">
        {sshCloneURL(repository, session) && (
          <div
            className="clone-protocol"
            role="group"
            aria-label="Clone protocol"
          >
            {(["HTTPS", "SSH"] as const).map((value) => (
              <button
                key={value}
                type="button"
                className="button"
                aria-pressed={protocol === value}
                onClick={() => {
                  onProtocolChange(value);
                  setStatus("");
                }}
              >
                {value}
              </button>
            ))}
          </div>
        )}
        <label htmlFor="repository-clone-url">{protocol} clone URL</label>
        <div className="copy-field">
          <input
            id="repository-clone-url"
            readOnly
            value={cloneURL}
            onFocus={(event) => event.target.select()}
          />
          <button className="button" type="button" onClick={copyURL}>
            Copy clone URL
          </button>
        </div>
        {status && (
          <p className="field-help" role="status">
            {status}
          </p>
        )}
        <pre tabIndex={0}>{`git clone ${shellQuote(cloneURL)}`}</pre>
        {protocol === "SSH" ? (
          <p className="field-help">
            <a href="/auth/ssh-keys">Add your public SSH key</a> in personal
            settings first. Use your GitOne username{" "}
            <strong>{session.username}</strong>, not “git” or the group name.
            Your current repository permissions apply. Keep the private key on
            your device.
          </p>
        ) : (
          <p className="field-help">
            When Git asks for credentials, use your personal username{" "}
            <strong>{session.username}</strong> and an{" "}
            <a href="/auth/tokens">access token</a> as the password. Select this
            repository when generating the token, or explicitly choose all
            repositories you have access to. Use read permission for clone/pull,
            or write permission for push.
          </p>
        )}
      </div>
    </details>
  );
}

function EmptyRepository({
  repository,
  session,
  protocol,
  onProtocolChange,
}: {
  repository: Repository;
  session: Session;
  protocol: CloneProtocol;
  onProtocolChange: (value: CloneProtocol) => void;
}) {
  const cloneURL = repositoryCloneURL(repository, session, protocol);
  const branch = shellQuote(repository.defaultBranch);
  const createCommands = [
    `mkdir ${shellQuote(repository.name)}`,
    `cd ${shellQuote(repository.name)}`,
    `git init -b ${branch}`,
    `printf ${shellQuote(`# ${repository.name}\\n`)} > README.md`,
    "git add README.md",
    'git commit -m "Initial commit"',
    `git remote add origin ${shellQuote(cloneURL)}`,
    `git push -u origin ${shellQuote(`HEAD:refs/heads/${repository.defaultBranch}`)}`,
  ].join("\n");
  return (
    <div className="empty-repository">
      <div className="panel empty-state">
        <div className="empty-icon">
          <RepoIcon />
        </div>
        <h2>This repository is empty</h2>
        <p>
          {repository.canWrite
            ? "Push your first commit from Git to add files, branches, and history."
            : "An owner or developer can push the first commit. You can clone this repository with your Git credentials."}
        </p>
      </div>
      <ClonePanel
        repository={repository}
        session={session}
        protocol={protocol}
        onProtocolChange={onProtocolChange}
        expanded
      />
      {repository.canWrite && (
        <section
          className="panel git-quickstart"
          aria-label="Repository quick setup"
        >
          <h2>Create a new repository locally</h2>
          <p className="muted">
            Run these commands in the parent directory where you want the
            project folder. Configure your Git author name and email first if
            needed.
          </p>
          <pre tabIndex={0}>{createCommands}</pre>
          <h2>Or push an existing repository</h2>
          <p className="muted">
            Run inside your existing local repository. If an origin remote
            already exists, update it instead of adding another.
          </p>
          <pre
            tabIndex={0}
          >{`git remote add origin ${shellQuote(cloneURL)}\ngit push -u origin HEAD`}</pre>
          <p className="field-help">
            {protocol === "SSH"
              ? "Use a registered SSH key for "
              : "Use a write token for "}
            <code>
              {repository.namespace}/{repository.name}
            </code>
            . Your group or personal access must also allow writes.
          </p>
        </section>
      )}
    </div>
  );
}

export function RepositoryPage({
  session,
  namespace,
  name,
}: {
  session: Session;
  namespace: string;
  name: string;
}) {
  const [repository, setRepository] = useState<Repository | null>(null);
  const [branches, setBranches] = useState<RepositoryBranch[]>([]);
  const [data, setData] = useState<BrowserData | null>(null);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [cloneProtocol, setCloneProtocol] = useState<CloneProtocol>("HTTPS");
  const query = new URLSearchParams(location.search);
  const selectedRef = query.get("ref") ?? repository?.defaultBranch ?? "main";
  const path = query.get("path") ?? "";
  const history = query.get("view") === "commits";
  const base = `/${namespace}/${name}`;
  const endpoint = `/repos/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
  function destination(nextPath = "", nextHistory = false, ref = selectedRef) {
    const params = new URLSearchParams({ ref });
    if (nextPath) params.set("path", nextPath);
    if (nextHistory) params.set("view", "commits");
    return `${base}?${params}`;
  }
  useEffect(() => {
    let active = true;
    setError("");
    Promise.all([
      api<Repository>(endpoint),
      api<{ branches: RepositoryBranch[] }>(`${endpoint}/branches`),
    ])
      .then(([repo, result]) => {
        if (active) {
          setRepository(repo);
          setBranches(result.branches);
        }
      })
      .catch((e) => {
        if (active) setError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, [endpoint, refresh]);
  useEffect(() => {
    if (!repository) return;
    let active = true;
    setData(null);
    async function load(): Promise<BrowserData> {
      if (repository!.empty) return {};
      const params = new URLSearchParams({ ref: selectedRef });
      if (history)
        return {
          commits: (
            await api<{ commits: RepositoryCommit[] }>(
              `${endpoint}/commits?${params}`,
            )
          ).commits,
        };
      if (path) {
        const parent = path.split("/").slice(0, -1).join("/");
        const listing = await api<RepositoryTree>(
          `${endpoint}/tree?${new URLSearchParams({ ref: selectedRef, path: parent })}`,
        );
        const entry = listing.entries.find((item) => item.path === path);
        if (!entry)
          throw new Error(
            "That file or directory does not exist on this branch. Return to the repository root or choose another branch.",
          );
        if (entry.type === "file")
          return {
            blob: await api<RepositoryBlob>(
              `${endpoint}/blob?${new URLSearchParams({ ref: selectedRef, path })}`,
            ),
          };
      }
      const tree = await api<RepositoryTree>(
        `${endpoint}/tree?${new URLSearchParams({ ref: selectedRef, path })}`,
      );
      const readmeEntry = tree.entries.find(
        (entry) =>
          entry.type === "file" && /^readme(?:\.md|\.txt)?$/i.test(entry.name),
      );
      const readme = readmeEntry
        ? await api<RepositoryBlob>(
            `${endpoint}/blob?${new URLSearchParams({ ref: selectedRef, path: readmeEntry.path })}`,
          )
        : undefined;
      return { tree, readme };
    }
    load()
      .then((result) => {
        if (active) setData(result);
      })
      .catch((e) => {
        if (active) setError(errorMessage(e));
      });
    return () => {
      active = false;
    };
  }, [repository, endpoint, selectedRef, path, history, refresh]);
  const pathParts = path.split("/").filter(Boolean);
  return (
    <main id="main" className="container repository-page">
      <div className="repository-heading">
        <RepoIcon />
        <h1>
          <a href={`/${namespace}`}>{namespace}</a>
          <span className="muted"> / </span>
          <a href={base}>{name}</a>
        </h1>
        {repository && <span className="badge">Private</span>}
      </div>
      {repository?.description && (
        <p className="repository-description">{repository.description}</p>
      )}
      <nav className="tabs" aria-label="Repository">
        <a href={destination()} aria-current={!history ? "page" : undefined}>
          Code
        </a>
        <a
          href={destination("", true)}
          aria-current={history ? "page" : undefined}
        >
          Commits
        </a>
      </nav>
      {error ? (
        <div className="repository-error">
          <ErrorNotice message={error} />
          <div className="form-actions">
            <button
              className="button"
              onClick={() => setRefresh((value) => value + 1)}
            >
              Try again
            </button>
            <a className="button" href={base}>
              Repository root
            </a>
            <a className="text-link" href="/">
              Your spaces
            </a>
          </div>
        </div>
      ) : !repository || !data ? (
        <Loading text="Loading repository…" />
      ) : repository.empty ? (
        <EmptyRepository
          repository={repository}
          session={session}
          protocol={cloneProtocol}
          onProtocolChange={setCloneProtocol}
        />
      ) : (
        <>
          <ClonePanel
            repository={repository}
            session={session}
            protocol={cloneProtocol}
            onProtocolChange={setCloneProtocol}
          />
          <div className="repository-toolbar">
            <div className="branch-control">
              <label htmlFor="repository-branch">Branch</label>
              <select
                id="repository-branch"
                value={selectedRef}
                onChange={(event) =>
                  location.assign(destination("", history, event.target.value))
                }
              >
                {!branches.some((branch) => branch.name === selectedRef) && (
                  <option value={selectedRef}>{selectedRef}</option>
                )}
                {branches.map((branch) => (
                  <option key={branch.name} value={branch.name}>
                    {branch.name}
                  </option>
                ))}
              </select>
            </div>
            <span className="muted">
              {branches.length} {branches.length === 1 ? "branch" : "branches"}
            </span>
            <span className="badge">{repository.role}</span>
          </div>
          {history ? (
            <section aria-label="Commit history">
              <h2 className="repository-subheading">Commit history</h2>
              {data.commits?.length ? (
                <div className="panel commit-list">
                  {data.commits.map((commit) => (
                    <article className="commit-row" key={commit.id}>
                      <div>
                        <h3>{commit.message}</h3>
                        <p className="muted">
                          {commit.authorName} committed on{" "}
                          {dateLabel(commit.createdAt)}
                        </p>
                      </div>
                      <code title={commit.id}>{commit.id.slice(0, 7)}</code>
                    </article>
                  ))}
                </div>
              ) : (
                <div className="panel compact-empty">
                  No commits on this branch.
                </div>
              )}
            </section>
          ) : (
            <>
              <nav className="repository-breadcrumbs" aria-label="File path">
                <a href={destination()}>{name}</a>
                {pathParts.map((part, index) => (
                  <span key={index}>
                    {" "}
                    /{" "}
                    {index === pathParts.length - 1 ? (
                      <strong>{part}</strong>
                    ) : (
                      <a
                        href={destination(
                          pathParts.slice(0, index + 1).join("/"),
                        )}
                      >
                        {part}
                      </a>
                    )}
                  </span>
                ))}
              </nav>
              {data.tree && (
                <div className="panel file-list">
                  <div className="file-list-heading">
                    <strong>Files</strong>
                    <a href={destination("", true)}>View commit history</a>
                  </div>
                  {path && (
                    <a
                      className="file-row"
                      href={destination(pathParts.slice(0, -1).join("/"))}
                    >
                      <RepoIcon directory />
                      <span>..</span>
                      <span className="sr-only">Parent directory</span>
                    </a>
                  )}
                  {data.tree.entries.length ? (
                    data.tree.entries.map((entry) => (
                      <a
                        className="file-row"
                        key={entry.path}
                        href={destination(entry.path)}
                      >
                        <RepoIcon directory={entry.type === "directory"} />
                        <span>{entry.name}</span>
                        <span className="file-size">
                          {entry.type === "directory"
                            ? "Directory"
                            : byteLabel(entry.size)}
                        </span>
                      </a>
                    ))
                  ) : (
                    <p className="compact-empty">This directory is empty.</p>
                  )}
                </div>
              )}
              {data.blob && (
                <section
                  className="panel file-preview"
                  aria-label="File contents"
                >
                  <div className="file-list-heading">
                    <strong>{pathParts.at(-1)}</strong>
                    <span className="muted">{byteLabel(data.blob.size)}</span>
                  </div>
                  {data.blob.binary ? (
                    <p className="compact-empty">
                      Binary file. A text preview is not available.
                    </p>
                  ) : (
                    <pre tabIndex={0}>{data.blob.content}</pre>
                  )}
                </section>
              )}
              {data.readme && !data.readme.binary && (
                <section
                  className="panel file-preview readme-preview"
                  aria-label="README"
                >
                  <div className="file-list-heading">
                    <strong>README</strong>
                    <a href={destination(data.readme.path)}>View file</a>
                  </div>
                  <pre tabIndex={0}>{data.readme.content}</pre>
                </section>
              )}
            </>
          )}
          <p className="field-help repository-access">
            Private repository ·{" "}
            {repository.canWrite
              ? "You have write access to this space."
              : "You have read-only access to this space."}{" "}
            Clone over HTTPS with an <a href="/auth/tokens">access token</a>
            {session.sshURL ? (
              <>
                {" "}
                or over SSH with a <a href="/auth/ssh-keys">registered key</a>.
              </>
            ) : (
              "."
            )}
          </p>
        </>
      )}
    </main>
  );
}
