import type { Session } from "./api";

export function pageTitle(
  pathname: string,
  search: string,
  session: Session | null,
  unavailable: boolean,
): string {
  if (unavailable) return "Service unavailable · GitOne";

  const path = pathname.replace(/\/$/, "") || "/";
  const [, name, subpath, action] = path.split("/");
  let label: string;

  if (path === "/") {
    label = session?.authenticated === false ? "Welcome" : "Your spaces";
  } else if (path === "/auth/login") {
    label = "Sign in";
  } else if (path === "/auth/register") {
    label = "Create account";
  } else if (path === "/auth/new-group") {
    label = "Create group";
  } else if (path === "/auth/new-repository") {
    label = "Create repository";
  } else if (path === "/auth/tokens") {
    label = "Access tokens";
  } else if (path === "/auth/ssh-keys") {
    label = "SSH keys";
  } else if (session?.authenticated && path === `/${session.username}`) {
    label = "Your spaces";
  } else if (subpath === "invitations" && action === "accept") {
    label = `Join /${name}`;
  } else if (subpath === "settings") {
    label = `Settings · /${name}`;
  } else if (subpath) {
    const repository = `${name}/${subpath}`;
    const query = new URLSearchParams(search);
    const file = query.get("path")?.split("/").filter(Boolean).pop();
    label = query.get("view") === "commits"
      ? `Commits · ${repository}`
      : file
        ? `${file} · ${repository}`
        : repository;
  } else {
    label = `/${name}`;
  }

  return `${label} · GitOne`;
}
