import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

export default defineConfig({
  // test/setup.ts stands in for DurableObjectNamespace.jurisdiction(), which workerd lacks.
  test: { setupFiles: ["./test/setup.ts"] },
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      // Artifacts has no local simulator, so with remote bindings on the pool
      // would open a session to real Cloudflare. Tests never do that: they
      // swap in test/fake-artifacts.ts via setRepoHostForTests().
      remoteBindings: false,
      miniflare: {
        // Test-only secret. Real values come from .dev.vars locally and
        // `wrangler secret put` in production (only with the maintainer's approval).
        // Access vars cleared: tests run as a self-host default and turn Access on per test.
        bindings: { WAYGLASS_ADMIN_KEY: "test-admin-key", ACCESS_TEAM_DOMAIN: "", ACCESS_AUD: "", ACCESS_WEB_AUD: "" },
      },
    }),
  ],
});
