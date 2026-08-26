// Tests for the persisted offline session snapshot: user scoping, corruption
// resistance, and the secret-free bootstrap reconstruction used for offline
// cold starts.

// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";
import type { Bootstrap } from "../types";
import { clearOfflineSessions, loadOfflineSession, offlineBootstrap, saveOfflineSession } from "./offlineSession";

function bootstrapFor(userID: number): Bootstrap {
  return {
    users_exist: true,
    csrf: `token-${userID}`,
    user: { id: userID, email: `user${userID}@example.test`, name: `User ${userID}` } as Bootstrap["user"],
    mailboxes: [{ id: 4, name: "INBOX" }] as Bootstrap["mailboxes"]
  };
}

beforeEach(() => {
  clearOfflineSessions();
  localStorage.clear();
});

describe("saveOfflineSession", () => {
  it("round-trips a signed-in session", () => {
    expect(saveOfflineSession(bootstrapFor(1))).toBe(true);
    const saved = loadOfflineSession();
    expect(saved?.user.id).toBe(1);
    expect(saved?.user.email).toBe("user1@example.test");
    expect(saved?.mailboxes).toHaveLength(1);
    expect(saved?.mailboxes[0].name).toBe("INBOX");
  });

  it("keeps exactly one session: saving another user replaces the first", () => {
    saveOfflineSession(bootstrapFor(1));
    saveOfflineSession(bootstrapFor(2));
    expect(loadOfflineSession()?.user.id).toBe(2);
    expect(localStorage.length).toBe(1);
  });

  it("refuses bootstraps without a signed-in user", () => {
    const anonymous = { ...bootstrapFor(1), user: null };
    expect(saveOfflineSession(anonymous)).toBe(false);
    expect(loadOfflineSession()).toBeNull();
  });
});

describe("loadOfflineSession", () => {
  it("survives corrupt JSON entries", () => {
    saveOfflineSession(bootstrapFor(3));
    const key = Object.keys(localStorage).find((key) => key.includes("offline.session"));
    localStorage.setItem(key as string, "{not json");
    expect(loadOfflineSession()).toBeNull();
  });

  it("rejects structurally invalid sessions instead of throwing", () => {
    const key = "rolltop.offline.session.v1.9";
    localStorage.setItem(key, JSON.stringify({ version: 1, saved_at: Date.now(), user: { id: "nope" } }));
    expect(loadOfflineSession()).toBeNull();
    localStorage.setItem(key, JSON.stringify({ version: 99, saved_at: Date.now(), user: { id: 9, email: "", name: "" }, mailboxes: [] }));
    expect(loadOfflineSession()).toBeNull();
  });
});

describe("clearOfflineSessions", () => {
  it("removes every stored session", () => {
    saveOfflineSession(bootstrapFor(5));
    clearOfflineSessions();
    expect(loadOfflineSession()).toBeNull();
    expect(localStorage.length).toBe(0);
  });
});

describe("offlineBootstrap", () => {
  it("reconstructs chrome state with no CSRF token and no live sync state", () => {
    saveOfflineSession(bootstrapFor(8));
    const session = loadOfflineSession() as NonNullable<ReturnType<typeof loadOfflineSession>>;
    const restored = offlineBootstrap(session);
    expect(restored.users_exist).toBe(true);
    // Writes must stay disabled until a live bootstrap supplies fresh CSRF.
    expect(restored.csrf).toBe("");
    expect(restored.user?.id).toBe(8);
    expect(restored.mailboxes).toHaveLength(1);
    expect(restored.latest_sync_run).toBeNull();
    expect(restored.active_sync_runs).toEqual([]);
    expect(restored.sync_running).toBe(false);
  });
});
