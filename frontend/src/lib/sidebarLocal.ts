// File overview: User-scoped persistence for which sidebar account groups are collapsed.
// Storage is best-effort; a missing or corrupt entry means every account stays expanded.

const collapsedAccountsPrefix = "rolltop.sidebar.collapsedAccounts.v1.";

function collapsedAccountsStorageKey(userID: number): string {
  return `${collapsedAccountsPrefix}${userID}`;
}

function positiveUserID(userID: number): boolean {
  return Number.isInteger(userID) && userID > 0;
}

export function loadCollapsedAccounts(userID: number): Set<string> {
  if (!positiveUserID(userID)) return new Set();
  try {
    const parsed = JSON.parse(localStorage.getItem(collapsedAccountsStorageKey(userID)) || "null") as unknown;
    if (Array.isArray(parsed)) return new Set(parsed.filter((key): key is string => typeof key === "string"));
  } catch {
    return new Set();
  }
  return new Set();
}

export function saveCollapsedAccounts(userID: number, collapsed: Set<string>): void {
  if (!positiveUserID(userID)) return;
  try {
    if (collapsed.size === 0) {
      localStorage.removeItem(collapsedAccountsStorageKey(userID));
      return;
    }
    localStorage.setItem(collapsedAccountsStorageKey(userID), JSON.stringify(Array.from(collapsed)));
  } catch {
    // Quota or privacy-mode failures leave the sidebar working without persistence.
  }
}

/** clearOtherCollapsedAccounts drops sidebar state belonging to other users on a shared browser. */
export function clearOtherCollapsedAccounts(userID: number): void {
  const keep = positiveUserID(userID) ? collapsedAccountsStorageKey(userID) : "";
  try {
    const stale: string[] = [];
    for (let index = 0; index < localStorage.length; index++) {
      const key = localStorage.key(index);
      if (key && key.startsWith(collapsedAccountsPrefix) && key !== keep) stale.push(key);
    }
    stale.forEach((key) => localStorage.removeItem(key));
  } catch {
    // Storage access failures leave the stale entries in place; they are inert.
  }
}
