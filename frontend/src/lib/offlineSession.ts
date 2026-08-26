// File overview: Persisted, secret-free session snapshot so a cold app start
// without connectivity can boot straight into cached mail instead of the login
// page. Only display identity data is stored; the CSRF token never persists.

import type { Bootstrap, Mailbox, User } from "../types";

const sessionVersion = 1;
const storageKeyPrefix = `rolltop.offline.session.v${sessionVersion}.`;

export type PersistedOfflineSession = {
  version: number;
  saved_at: number;
  user: User;
  mailboxes: Mailbox[];
};

export function saveOfflineSession(bootstrap: Bootstrap): boolean {
  const user = bootstrap.user;
  if (!user || !(user.id > 0)) return false;
  const session: PersistedOfflineSession = {
    version: sessionVersion,
    saved_at: Date.now(),
    user,
    mailboxes: Array.isArray(bootstrap.mailboxes) ? bootstrap.mailboxes : []
  };
  try {
    // One session at a time: switching accounts must not leave the previous
    // identity readable from storage before the new bootstrap confirms it.
    for (const key of Object.keys(localStorage)) {
      if (key.startsWith(storageKeyPrefix)) localStorage.removeItem(key);
    }
    localStorage.setItem(storageKey(user.id), JSON.stringify(session));
    return true;
  } catch {
    return false;
  }
}

export function loadOfflineSession(): PersistedOfflineSession | null {
  try {
    for (const key of Object.keys(localStorage)) {
      if (!key.startsWith(storageKeyPrefix)) continue;
      let parsed: unknown;
      try {
        parsed = JSON.parse(localStorage.getItem(key) || "null");
      } catch {
        continue;
      }
      if (validSession(parsed)) return parsed;
    }
  } catch {
    // Storage may be unavailable in private or locked-down browser contexts.
  }
  return null;
}

export function clearOfflineSessions(): void {
  try {
    for (const key of Object.keys(localStorage)) {
      if (key.startsWith(storageKeyPrefix)) localStorage.removeItem(key);
    }
  } catch {
    // Best-effort cleanup only.
  }
}

/**
 * offlineBootstrap reconstructs enough chrome state to render cached views.
 * Writes stay disabled until a live bootstrap supplies a fresh CSRF token.
 */
export function offlineBootstrap(session: PersistedOfflineSession): Bootstrap {
  return {
    users_exist: true,
    csrf: "",
    user: session.user,
    mailboxes: session.mailboxes,
    latest_sync_run: null,
    active_sync_runs: [],
    sync_running: false
  };
}

function storageKey(userID: number): string {
  return `${storageKeyPrefix}${userID}`;
}

function validSession(value: unknown): value is PersistedOfflineSession {
  if (!value || typeof value !== "object") return false;
  const session = value as Partial<PersistedOfflineSession>;
  if (session.version !== sessionVersion || !Number.isFinite(session.saved_at)) return false;
  const user = session.user as Partial<User> | undefined;
  if (!user || typeof user.id !== "number" || !(user.id > 0) || typeof user.email !== "string" || typeof user.name !== "string") return false;
  return Array.isArray(session.mailboxes);
}
