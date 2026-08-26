// File overview: IndexedDB engine primitives shared by Rolltop's offline
// stores. This module owns database open/upgrade and generic request helpers;
// it knows nothing about mail or outbox records. All operations are best-effort
// and resolve to empty results instead of throwing when storage is unavailable.

export const OFFLINE_DB_NAME = "rolltop.offline";
export const OFFLINE_DB_VERSION = 1;
export const HEADERS_STORE = "headers";
export const BODIES_STORE = "bodies";
export const OUTBOX_STORE = "outbox";

let openPromise: Promise<IDBDatabase | null> | null = null;

/** openOfflineDB resolves the shared connection, or null when IDB is unusable. */
export function openOfflineDB(): Promise<IDBDatabase | null> {
  if (openPromise) return openPromise;
  openPromise = new Promise<IDBDatabase | null>((resolve) => {
    try {
      if (typeof indexedDB === "undefined") {
        resolve(null);
        return;
      }
      const request = indexedDB.open(OFFLINE_DB_NAME, OFFLINE_DB_VERSION);
      request.onupgradeneeded = () => {
        const db = request.result;
        if (!db.objectStoreNames.contains(HEADERS_STORE)) {
          db.createObjectStore(HEADERS_STORE, { keyPath: ["user_id", "key"] }).createIndex("viewed_at", "viewed_at");
        }
        if (!db.objectStoreNames.contains(BODIES_STORE)) {
          const bodies = db.createObjectStore(BODIES_STORE, { keyPath: ["user_id", "root_id"] });
          bodies.createIndex("message_id", "thread_ids", { multiEntry: true });
          bodies.createIndex("viewed_at", "viewed_at");
        }
        if (!db.objectStoreNames.contains(OUTBOX_STORE)) {
          const outbox = db.createObjectStore(OUTBOX_STORE, { keyPath: "id", autoIncrement: true });
          outbox.createIndex("user_status", ["user_id", "status"]);
        }
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => resolve(null);
      request.onblocked = () => resolve(null);
    } catch {
      resolve(null);
    }
  });
  return openPromise;
}

/**
 * runInTransaction executes one best-effort transaction and resolves once the
 * transaction settles. Requests must be queued synchronously inside `run`;
 * follow-up work belongs in request onsuccess handlers so the transaction
 * stays alive until it finishes.
 */
export function runInTransaction(stores: string[], mode: IDBTransactionMode, run: (transaction: IDBTransaction) => void): Promise<boolean> {
  return new Promise<boolean>((resolve) => {
    openOfflineDB().then((db) => {
      if (!db) {
        resolve(false);
        return;
      }
      let transaction: IDBTransaction;
      try {
        transaction = db.transaction(stores, mode);
      } catch {
        resolve(false);
        return;
      }
      transaction.oncomplete = () => resolve(true);
      transaction.onerror = () => resolve(false);
      transaction.onabort = () => resolve(false);
      try {
        run(transaction);
      } catch {
        try {
          transaction.abort();
        } catch {
          // Already committed or aborted; nothing left to clean up.
        }
      }
    });
  });
}

/** getAllFromStore resolves every record in one store, optionally range-limited. */
export function getAllFromStore<T>(storeName: string, range?: IDBKeyRange): Promise<T[]> {
  return promiseRequest<T[]>(openOfflineDB().then((db) => {
    if (!db) return null;
    return db.transaction([storeName], "readonly").objectStore(storeName).getAll(range);
  }), []);
}

/** getAllFromIndex resolves records through one store index, optionally range-limited. */
export function getAllFromIndex<T>(storeName: string, indexName: string, range?: IDBKeyRange): Promise<T[]> {
  return promiseRequest<T[]>(openOfflineDB().then((db) => {
    if (!db) return null;
    return db.transaction([storeName], "readonly").objectStore(storeName).index(indexName).getAll(range);
  }), []);
}

/** getFromStore resolves one record by primary key. */
export function getFromStore<T>(storeName: string, key: IDBValidKey): Promise<T | null> {
  return promiseRequest<T | null>(openOfflineDB().then((db) => {
    if (!db) return null;
    return db.transaction([storeName], "readonly").objectStore(storeName).get(key);
  }), null);
}

/** putInStore upserts one record by primary key. */
export async function putInStore(storeName: string, value: unknown): Promise<boolean> {
  return runInTransaction([storeName], "readwrite", (transaction) => {
    transaction.objectStore(storeName).put(value);
  });
}

/** deleteFromStore removes one record by primary key. */
export async function deleteFromStore(storeName: string, key: IDBValidKey): Promise<void> {
  await runInTransaction([storeName], "readwrite", (transaction) => {
    transaction.objectStore(storeName).delete(key);
  });
}

function promiseRequest<T>(requestPromise: Promise<IDBRequest | null>, fallback: T): Promise<T> {
  return requestPromise.then((request) => new Promise<T>((resolve) => {
    if (!request) {
      resolve(fallback);
      return;
    }
    request.onsuccess = () => resolve(request.result as T);
    request.onerror = () => resolve(fallback);
  })).catch(() => fallback);
}

/**
 * visitCursorEntries drives one cursor request, invoking `visit` per entry
 * until exhaustion. Returning false from `visit` stops iteration early; a
 * throwing visitor aborts through the transaction's normal error path.
 */
export function visitCursorEntries(cursor: IDBRequest<IDBCursorWithValue | null>, visit: (cursor: IDBCursorWithValue) => boolean | void): void {
  cursor.onsuccess = () => {
    const value = cursor.result;
    if (!value) return;
    if (visit(value) !== false) value.continue();
  };
}

/** userKeyRange bounds a compound [user_id, ...] key to one user's rows. */
export function userKeyRange(userID: number): IDBKeyRange {
  return IDBKeyRange.bound([userID, 0], [userID, Number.MAX_SAFE_INTEGER]);
}
