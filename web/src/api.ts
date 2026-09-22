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
export const errorMessage = (error: unknown) =>
  error instanceof Error
    ? error.message
    : "Something went wrong. Please try again.";
export function safeReturnTo(value: string | null): string {
  if (!value || value === "/" || value === "/auth/new-group")
    return value || "/";
  if (!value.startsWith("/")) return "/";
  const name = value.slice(1).split("/")[0];
  if (!validName(name) || ["api", "auth", "gitone", "system"].includes(name))
    return "/";
  const suffix = value.slice(name.length + 1).replace(/\/$/, "");
  return ["", "/settings", "/invitations/accept"].includes(suffix)
    ? value
    : "/";
}
