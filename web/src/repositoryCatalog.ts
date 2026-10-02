import { api, validRepositoryName } from "./api";
import type { Repository, RepositoryList } from "./api";

export async function repositoryPage(
  namespace: string,
  after: string | undefined,
  signal: AbortSignal,
): Promise<RepositoryList> {
  signal.throwIfAborted();
  const query = after ? `?after=${encodeURIComponent(after)}` : "";
  const page = await api<RepositoryList>(
    `/repos/${encodeURIComponent(namespace)}${query}`,
    { signal: AbortSignal.any([signal, AbortSignal.timeout(20000)]) },
  );
  signal.throwIfAborted();
  if (!Array.isArray(page.repositories))
    throw new Error(
      "Repository listing returned an invalid page. Please try again.",
    );
  if (
    page.nextCursor !== undefined &&
    (typeof page.nextCursor !== "string" || !validRepositoryName(page.nextCursor))
  )
    throw new Error(
      "Repository listing returned an invalid pagination cursor. Please try again.",
    );
  return page;
}

export function mergeRepositories(
  current: Repository[],
  incoming: Repository[],
): Repository[] {
  return [
    ...new Map(
      [...current, ...incoming].map((repo) => [repo.id, repo]),
    ).values(),
  ];
}

// Token scopes need the complete catalog. Never present a partial result as
// complete, and stop both canceled requests and repeated pagination cursors.
export async function loadRepositoryCatalog(
  namespace: string,
  signal: AbortSignal,
): Promise<Repository[]> {
  let repositories: Repository[] = [];
  let after: string | undefined;
  const seen = new Set<string>();
  for (;;) {
    const page = await repositoryPage(namespace, after, signal);
    repositories = mergeRepositories(repositories, page.repositories);
    if (page.nextCursor === undefined) return repositories;
    if (seen.has(page.nextCursor))
      throw new Error(
        "Repository listing returned a repeated pagination cursor. Please try again.",
      );
    seen.add(page.nextCursor);
    after = page.nextCursor;
  }
}
