import { api } from "./api";
import type { Space } from "./api";

interface SpacePage {
  spaces: Space[];
  nextCursor?: string;
}

// Publish only a complete discovery result. An empty filtered page can still
// have a cursor when the backend skipped stale membership candidates.
export async function loadSpaces(
  shardCount: number,
  signal: AbortSignal,
): Promise<Space[]> {
  const controller = new AbortController();
  const discoverySignal = AbortSignal.any([signal, controller.signal]);
  async function loadShard(shard: number): Promise<Space[]> {
    const spaces: Space[] = [];
    const seen = new Set<string>();
    let cursor: string | undefined;
    for (;;) {
      discoverySignal.throwIfAborted();
      const query = new URLSearchParams({ shard: String(shard), limit: "100" });
      if (cursor !== undefined) query.set("cursor", cursor);
      const page = await api<SpacePage>(`/spaces?${query}`, {
        signal: AbortSignal.any([discoverySignal, AbortSignal.timeout(20000)]),
      });
      discoverySignal.throwIfAborted();
      if (!Array.isArray(page.spaces))
        throw new Error(
          "Space discovery returned an invalid page. Please try again.",
        );
      spaces.push(...page.spaces);
      if (page.nextCursor === undefined) return spaces;
      if (typeof page.nextCursor !== "string" || !page.nextCursor)
        throw new Error(
          "Space discovery returned an invalid pagination cursor. Please try again.",
        );
      if (seen.has(page.nextCursor))
        throw new Error(
          "Space discovery returned a repeated pagination cursor. Please try again.",
        );
      seen.add(page.nextCursor);
      cursor = page.nextCursor;
    }
  }
  try {
    const pages = await Promise.all(
      Array.from({ length: shardCount }, (_, shard) => loadShard(shard)),
    );
    return pages.flat().sort((a, b) => a.name.localeCompare(b.name));
  } catch (error) {
    // Stop other shards as soon as one fails; partial membership is not success.
    controller.abort();
    throw error;
  }
}
