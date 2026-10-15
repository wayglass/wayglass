// Runs before every test file (vitest.config.ts setupFiles).
//
// workerd does not implement DurableObjectNamespace.jurisdiction(), so the
// pinned namespace a project in a pinned jurisdiction gets (src/residency.ts)
// is replaced here by one built on the unpinned namespace under a prefixed
// name: a different object from the unpinned one, and every id it hands out
// is recorded, so a test can assert which namespace served a project.

import { env } from "cloudflare:workers";
import { beforeEach } from "vitest";
import { resetResidencyCacheForTests, setPinnedNamespaceForTests } from "../src/residency";

/** Every id a pinned namespace handed out, in order: [jurisdiction, project name]. */
export const pinnedIds: [string, string][] = [];

setPinnedNamespaceForTests((jurisdiction) => ({
  idFromName(name: string) {
    pinnedIds.push([jurisdiction, name]);
    return env.PROJECT.idFromName(`pinned:${jurisdiction}:${name}`);
  },
  get(id: DurableObjectId) {
    return env.PROJECT.get(id);
  },
}));

beforeEach(() => {
  pinnedIds.length = 0;
  resetResidencyCacheForTests();
});
