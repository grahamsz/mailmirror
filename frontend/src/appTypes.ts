// File overview: View-only TypeScript types shared inside the React app, separate from API response
// types so navigation, toasts, and shell callbacks do not leak into backend contracts.

import type { ReactNode } from "react";
import type { Bootstrap, Mailbox, OutboxSummary, User } from "./types";

/** LocationState is the minimal browser URL state App passes through the manual router. */
export type LocationState = {
  path: string;
  search: string;
};

/** Toast is a transient global notification rendered by the root ToastStack. */
export type Toast = {
  id: number;
  kind: "loading" | "success" | "error";
  message: string;
  action?: {
    label: string;
    onClick: () => void;
  };
};

export type ToastCommitReason = "dismiss" | "timeout" | "background";

/** ToastUndo defers a mutation until its toast settles, giving the user a real cancellation window. */
export type ToastUndo = {
  label?: string;
  onUndo: () => void;
  onCommit: (reason: ToastCommitReason) => void;
};

/** MoveTarget identifies the destination mailbox for drag/drop message transfers. */
export type MoveTarget = {
  id: number;
  name: string;
};

export type MessageTransferAction = "move" | "copy";

export type UnlockedSecurityKey = {
  id: number;
  identity_id: number;
  label: string;
  fingerprint: string;
  public_key_armored: string;
  algorithm?: string;
  key_id?: string;
  encryption_key_id?: string;
  privateKey: unknown;
};

export type SecurityUnlockState = {
  unlockedUntil: number;
  keys: UnlockedSecurityKey[];
};

export type OpenSecurityUnlock = (identityID?: number, onUnlocked?: (state: SecurityUnlockState) => void, recipientKeyIDs?: string[], fallbackEmail?: string) => void;

/** DatePrefs is the subset of user preferences required by date-formatting helpers. */
export type DatePrefs = Pick<User, "date_locale" | "date_format">;

/** Navigate pushes a client-side URL without reloading the Go-served SPA. */
export type Navigate = (url: string) => void;
/** AddToast enqueues a global toast and returns its generated ID. */
export type AddToast = (message: string, kind?: Toast["kind"], undo?: ToastUndo) => number;
/** RefreshChrome reloads bootstrap/chrome state after mutations. */
export type RefreshChrome = () => Promise<Bootstrap | null>;

/** AppShellProps collects authenticated chrome state and shell callbacks shared across views. */
export type AppShellProps = {
  user: User;
  csrf: string;
  mailboxes: Mailbox[];
  latestSyncRun: import("./types").SyncRun | null;
  activeSyncRuns: import("./types").SyncRun[];
  syncRunning: boolean;
  outbox: OutboxSummary;
  accountNeedsPassword: boolean;
  accountNotice: string;
  enabledPlugins: string[];
  serverStartedAt: string;
  serverUptimeSeconds: number;
  buildVersion: string;
  buildDate: string;
  buildLabel: string;
  buildCommit: string;
  location: LocationState;
  navigate: Navigate;
  onMoveMessages: (messageIDs: number[], mailbox: MoveTarget, action?: MessageTransferAction) => void;
  openCompose: (query?: string) => void;
  refreshChrome: RefreshChrome;
  notificationsEnabled: boolean;
  toggleNotifications: () => Promise<void>;
  securityUnlockAvailable: boolean;
  securityUnlock: SecurityUnlockState;
  openSecurityUnlock: OpenSecurityUnlock;
  lockSecurity: () => void;
  logout: () => Promise<void>;
  children: ReactNode;
};
