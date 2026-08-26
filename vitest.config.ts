import { defineConfig } from "vitest/config";

// Separate from vite.config.ts because that file is rooted at frontend/ for
// the app build, while tests live beside the modules they cover and run from
// the repository root. DOM-dependent suites opt in per file with a
// "@vitest-environment jsdom" docblock so everything else stays on fast node.
export default defineConfig({
  test: {
    include: ["frontend/src/**/*.test.ts"],
    environment: "node",
    environmentOptions: {
      // jsdom disables Web Storage on opaque origins; give tests a real one.
      jsdom: { url: "http://localhost/" }
    },
    setupFiles: ["frontend/src/test/setup.ts"]
  }
});
