// Shared vitest setup: guarantees a working Web Storage implementation.
// Node >= 22 exposes an inert global localStorage unless --localstorage-file
// is passed, and some jsdom builds skip storage entirely, which would silently
// break any suite touching browser persistence. Install a small Map-backed
// shim whenever the real thing cannot complete a write cycle.
const probeKey = "__rolltop_storage_probe__";

// Node >= 22 exposes its experimental localStorage as a lazy getter that
// warns on every access; detect that shape instead of touching it.
const descriptor = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
const nodeExperimentalStorage = Boolean(descriptor?.get && !descriptor.value);

let storageUsable = false;
if (!nodeExperimentalStorage) {
  try {
    globalThis.localStorage.setItem(probeKey, "1");
    storageUsable = globalThis.localStorage.getItem(probeKey) === "1";
    globalThis.localStorage.removeItem(probeKey);
  } catch {
    storageUsable = false;
  }
}

if (!storageUsable) {
  const backing = new Map<string, string>();
  const shim = {
    get length() {
      return backing.size;
    },
    clear: () => {
      for (const key of Array.from(backing.keys())) delete (shim as Record<string, unknown>)[key];
      backing.clear();
    },
    getItem: (key: string) => (backing.has(key) ? (backing.get(key) as string) : null),
    key: (index: number) => Array.from(backing.keys())[index] ?? null,
    removeItem: (key: string) => {
      backing.delete(key);
      delete (shim as Record<string, unknown>)[key];
    },
    setItem: (key: string, value: string) => {
      backing.set(key, String(value));
      // Real Storage surfaces keys as enumerable own properties, which
      // Object.keys(localStorage) relies on.
      (shim as Record<string, unknown>)[key] = String(value);
    }
  };
  globalThis.localStorage = shim as unknown as typeof globalThis.localStorage;
  Object.defineProperty(globalThis, "localStorage", { value: shim, configurable: true });
}
