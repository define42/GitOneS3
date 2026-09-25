export type Role = "owner" | "developer" | "reader";
export interface Session {
  authenticated: boolean;
  username?: string;
  identity?: { email: string; subject: string; issuer?: string };
  userId?: string;
  csrfToken?: string;
  shardCount: number;
  provider: string;
}
export interface Space {
  name: string;
  type: "group";
  role: Role;
  invited: boolean;
}
export interface Group {
  name: string;
  type: "group";
  creatorUserId: string;
  role: Role;
  members: Record<string, Role>;
  invitations?: Record<string, Role>;
  csrfToken: string;
}
export interface Repository {
  id: string;
  namespace: string;
  name: string;
  description: string;
  defaultBranch: string;
  createdAt: string;
  createdBy: string;
  visibility: "private";
  empty: boolean;
  role: Role;
  canWrite: boolean;
}
export interface RepositoryList {
  repositories: Repository[];
  role: Role;
  canWrite: boolean;
}
export interface RepositoryBranch {
  name: string;
  commit: string;
}
export interface RepositoryTree {
  ref: string;
  path: string;
  commit: string;
  entries: {
    name: string;
    path: string;
    type: "file" | "directory";
    size: number;
  }[];
}
export interface RepositoryBlob {
  ref: string;
  path: string;
  commit: string;
  content: string;
  size: number;
  binary: boolean;
}
export interface RepositoryCommit {
  id: string;
  message: string;
  authorName: string;
  createdAt: string;
  parents: string[];
}
export type TokenPermission = "read" | "write";
export interface AccessToken {
  id: string;
  name: string;
  username: string;
  permission: TokenPermission;
  repositories: string[];
  allRepositories?: boolean;
  createdAt: string;
  expiresAt: string;
  revokedAt?: string;
}
export interface CreatedAccessToken {
  token: string;
  metadata: AccessToken;
}
export class APIError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}
export async function api<T>(
  path: string,
  options: {
    method?: string;
    body?: unknown;
    csrf?: string;
    signal?: AbortSignal;
  } = {},
): Promise<T> {
  const response = await fetch(`/api/v1${path}`, {
    method: options.method ?? "GET",
    credentials: "same-origin",
    headers: {
      Accept: "application/json",
      ...(options.body !== undefined
        ? { "Content-Type": "application/json" }
        : {}),
      ...(options.csrf ? { "X-CSRF-Token": options.csrf } : {}),
    },
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
    signal: options.signal ?? AbortSignal.timeout(20000),
  });
  if (!response.ok) {
    const problem = (await response.json().catch(() => null)) as {
      detail?: string;
      title?: string;
    } | null;
    throw new APIError(
      response.status,
      problem?.detail ??
        problem?.title ??
        `Request failed (${response.status}). Please try again.`,
    );
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}
export const validName = (name: string) =>
  name.length <= 63 && /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(name);
export const validRepositoryName = (name: string) =>
  name.length <= 63 &&
  /^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$/.test(name) &&
  !name.includes("..") &&
  !name.endsWith(".git") &&
  !["auth", "settings", "members", "invitations"].includes(name);
export const errorMessage = (error: unknown) =>
  error instanceof Error
    ? error.message
    : "Something went wrong. Please try again.";
export function safeReturnTo(value: string | null): string {
  if (!value) return "/";
  if (new TextEncoder().encode(value).length > 1024) return "/";
  // Match Go's JSON escaping before this value enters the signed login state.
  const serialized = JSON.stringify(value).replace(
    /[<>&\u2028\u2029]/g,
    (character) =>
      "\\u" + character.charCodeAt(0).toString(16).padStart(4, "0"),
  );
  if (new TextEncoder().encode(serialized).length > 1024) return "/";
  if (
    !value.startsWith("/") ||
    value.startsWith("//") ||
    /[\\#\u0000-\u0020]/.test(value)
  )
    return "/";
  const [path, query = "", extra] = value.split("?");
  if (extra !== undefined) return "/";
  if (path.includes("%")) return "/";
  const params = new URLSearchParams(query);
  const validQuery = (allowed: string[]) =>
    [...params].every(
      ([key, entry]) =>
        allowed.includes(key) &&
        params.getAll(key).length === 1 &&
        !/[\\\u0000-\u001f\u007f]/.test(entry),
    );
  if (["/", "/auth/new-group", "/auth/tokens"].includes(path))
    return query ? "/" : value;
  if (path === "/auth/new-repository")
    return validQuery(["namespace"]) &&
      (!params.has("namespace") ||
        (validName(params.get("namespace")!) &&
          !["api", "auth", "gitone", "system"].includes(
            params.get("namespace")!,
          )))
      ? value
      : "/";
  const name = path.slice(1).split("/")[0];
  if (!validName(name) || ["api", "auth", "gitone", "system"].includes(name))
    return "/";
  const suffix = path.slice(name.length + 1).replace(/\/$/, "");
  if (["", "/settings", "/invitations/accept"].includes(suffix))
    return query ? "/" : value;
  return suffix.startsWith("/") &&
    validRepositoryName(suffix.slice(1)) &&
    validQuery(["ref", "path", "view"]) &&
    (params.get("ref")?.length ?? 0) <= 128 &&
    (params.get("path")?.length ?? 0) <= 4096 &&
    (!params.has("view") || params.get("view") === "commits")
    ? value
    : "/";
}
