// File overview: Settings surface for profile preferences, IMAP servers, SMTP servers, outgoing
// identities, folder sync/indexing controls, storage usage, and admin plugin panels.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api } from "../../api";
import type { DatePrefs, LocationState, Toast } from "../../appTypes";
import type { Account, AccountPurgeEstimate, Bootstrap, FolderProgress, MailIdentity, PluginSetting, Mailbox, SMTPAccount, StorageStats, SwipeAction, SwipePreferences, SwipeSnoozePreset, SyncFolder, SyncRun, SyncRunLiveDetail, ThemeDefinition, User } from "../../types";
import { Icon } from "../../components/Icon";
import { Field, Stat } from "../../components/common";
import { emptyAccountForm, accountToForm } from "../../lib/accountForm";
import { messageFromError } from "../../lib/errors";
import { displayDateTime, displayTime, formatBytes } from "../../lib/format";
import { folderParentNames, folderTree, trashMailboxForAccount, type FolderNode } from "../../lib/folders";
import { effectiveMailboxSyncMode, mergeSyncRuns } from "../../lib/sync";
import { swipeActionChoices, swipeSnoozeChoices } from "../../lib/swipeActions";
import { pluginIDs } from "../../plugins/registry";
import { accountSettingsRoutes, matchAccountSettingsRoute } from "../../plugins/runtime";
import type { RuntimePlugin, RuntimePlugins } from "../../plugins/runtime";
import { identitySecuritySettings } from "../../plugins/identitySecurity";
import { AdminRemoteImageBlocklist } from "../../plugins/remoteImageBlocklist/AdminRemoteImageBlocklist";
import { PluginTogglePanel } from "./admin/PluginTogglePanel";
import { SettingsEmpty, SettingsError, SettingsIndex, SettingsIndexRow, SettingsLoading, SettingsPage, SettingsShell } from "./SettingsUI";
import type { SettingsSectionID } from "./SettingsUI";

function emptySMTPForm() {
  return {
    label: "",
    host: "",
    port: "587",
    username: "",
    password: "",
    use_tls: true
  };
}

function smtpToForm(account: SMTPAccount | null) {
  if (!account) return emptySMTPForm();
  return {
    label: account.label || "",
    host: account.host || "",
    port: String(account.port || 587),
    username: account.username || "",
    password: "",
    use_tls: account.use_tls
  };
}

type StorageIndexBreakdownView = {
  FileCount?: unknown;
  ZapCount?: unknown;
  ZapBytes?: unknown;
  LargestZapPath?: unknown;
  LargestZapBytes?: unknown;
  RootBytes?: unknown;
  OtherBytes?: unknown;
};

function storageIndexBreakdown(stats: StorageStats): StorageIndexBreakdownView {
  const value = stats.IndexBreakdown;
  return value && typeof value === "object" ? value as StorageIndexBreakdownView : {};
}

function formatStatCount(value: unknown): string {
  const count = typeof value === "number" ? value : Number(value || 0);
  return Number.isFinite(count) ? count.toLocaleString() : "0";
}

function statDetail(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function imapDeletePrompt(estimate: AccountPurgeEstimate): string {
  return [
    `Remove the IMAP server "${estimate.account_name}" from rolltop?`,
    "",
    "This does not delete any messages from the remote IMAP server.",
    "rolltop will hide the server now, then purge local SQLite rows, message bodies, and full-text index documents in the background.",
    "",
    `Folders: ${formatStatCount(estimate.mailbox_count)}`,
    `Message headers: ${formatStatCount(estimate.message_count)}`,
    `Message bodies: ${formatStatCount(estimate.blob_count)} blobs, ${formatBytes(estimate.blob_bytes)}`,
    `Full text index: ${formatStatCount(estimate.search_index_count)} documents`,
    "",
    `Type ${estimate.account_name} to confirm.`
  ].join("\n");
}

function smtpDeleteConfirmationName(account: SMTPAccount): string {
  return account.label.trim() || account.host.trim() || "SMTP server";
}

function hasStorageIndexBreakdown(value: StorageIndexBreakdownView): boolean {
  return Boolean(value.FileCount || value.ZapCount || value.ZapBytes || value.LargestZapBytes || value.RootBytes || value.OtherBytes);
}

function searchPresetDefaults(preset: string) {
  switch (preset) {
    case "strict":
      return {
        search_preset: "strict",
        search_recency_bias: "light",
        search_fuzzy: "off",
        search_sender_boost: true,
        search_sender_history: "light",
        search_contact_boost: "light",
        search_attachment_weight: "normal",
        search_compact_splitting: false
      };
    case "forgiving":
      return {
        search_preset: "forgiving",
        search_recency_bias: "normal",
        search_fuzzy: "forgiving",
        search_sender_boost: true,
        search_sender_history: "strong",
        search_contact_boost: "strong",
        search_attachment_weight: "strong",
        search_compact_splitting: true
      };
    default:
      return {
        search_preset: "balanced",
        search_recency_bias: "normal",
        search_fuzzy: "balanced",
        search_sender_boost: true,
        search_sender_history: "normal",
        search_contact_boost: "normal",
        search_attachment_weight: "normal",
        search_compact_splitting: true
      };
  }
}

function normalizeIdentityKey(value: string) {
  return value.trim().toLowerCase();
}

function identityAccountMatches(identity: MailIdentity, account: Account | undefined, smtp: SMTPAccount | undefined) {
  if (!account) return false;
  const keys = new Set([identity.email, smtp?.username || ""].map(normalizeIdentityKey).filter(Boolean));
  if (keys.size === 0) return false;
  return [account.email, account.username, account.smtp_username].some((value) => keys.has(normalizeIdentityKey(value || "")));
}

function imapAccountLabel(account: Account) {
  return account.label || account.email || account.host || "IMAP server";
}

function smtpAccountLabel(account: SMTPAccount) {
  return account.label || account.username || account.host || "SMTP server";
}

function mailboxChoiceLabel(mailbox: Mailbox, accounts: Account[]) {
  const account = accounts.find((item) => item.id === mailbox.account_id);
  const accountLabel = account ? imapAccountLabel(account) : mailbox.account_label || mailbox.account_email || "IMAP";
  return `${accountLabel} / ${mailbox.name}`;
}

function firstMailboxIDForAccountRole(mailboxes: Mailbox[], accountID: number, role: string) {
  return mailboxes.find((mailbox) => mailbox.account_id === accountID && mailbox.role === role)?.id || 0;
}

function identityMailboxChoices(identity: MailIdentity, role: string, mailboxes: Mailbox[], accounts: Account[], smtpAccounts: SMTPAccount[]) {
  const roleMatches = mailboxes.filter((mailbox) => mailbox.role === role);
  if (identity.imap_account_id) {
    return roleMatches.filter((mailbox) => mailbox.account_id === identity.imap_account_id);
  }
  const smtp = smtpAccounts.find((item) => item.id === identity.smtp_account_id);
  const matchingAccount = roleMatches.filter((mailbox) => identityAccountMatches(identity, accounts.find((account) => account.id === mailbox.account_id), smtp));
  return matchingAccount.length > 0 ? matchingAccount : roleMatches;
}

function RichSignatureEditor({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const editorRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const editor = editorRef.current;
    if (!editor || document.activeElement === editor || editor.innerHTML === value) return;
    editor.innerHTML = value || "";
  }, [value]);

  function format(command: string, commandValue?: string) {
    const editor = editorRef.current;
    if (!editor) return;
    editor.focus();
    document.execCommand(command, false, commandValue);
    onChange(editor.innerHTML);
  }

  function addLink() {
    const href = window.prompt("Link URL");
    if (!href) return;
    format("createLink", href);
  }

  return (
    <div className="rich-signature-editor">
      <div className="rich-signature-toolbar" aria-label="Signature formatting">
        <button type="button" className="ghost" onClick={() => format("bold")}><strong>B</strong></button>
        <button type="button" className="ghost" onClick={() => format("italic")}><em>I</em></button>
        <button type="button" className="ghost" onClick={() => format("insertUnorderedList")}><Icon name="format_list_bulleted" /></button>
        <button type="button" className="ghost" onClick={addLink}><Icon name="link" /></button>
      </div>
      <div
        ref={editorRef}
        className="rich-signature-input"
        contentEditable
        role="textbox"
        aria-multiline="true"
        suppressContentEditableWarning
        onInput={(event) => onChange(event.currentTarget.innerHTML)}
        onBlur={(event) => onChange(event.currentTarget.innerHTML)}
      />
    </div>
  );
}

function IdentityMailboxFields({
  identity,
  accounts,
  smtpAccounts,
  mailboxes,
  updateIdentity
}: {
  identity: MailIdentity;
  accounts: Account[];
  smtpAccounts: SMTPAccount[];
  mailboxes: Mailbox[];
  updateIdentity: (id: number, patch: Partial<MailIdentity>) => void;
}) {
  const sentChoices = identityMailboxChoices(identity, "sent", mailboxes, accounts, smtpAccounts);
  const draftsChoices = identityMailboxChoices(identity, "drafts", mailboxes, accounts, smtpAccounts);

  function changeIMAPAccount(accountID: number) {
    updateIdentity(identity.id, {
      imap_account_id: accountID,
      sent_mailbox_id: accountID ? firstMailboxIDForAccountRole(mailboxes, accountID, "sent") : 0,
      drafts_mailbox_id: accountID ? firstMailboxIDForAccountRole(mailboxes, accountID, "drafts") : 0
    });
  }

  return (
    <div className="identity-mailbox-fields">
      <div>
        <label>IMAP server</label>
        <select value={identity.imap_account_id || 0} onChange={(event) => changeIMAPAccount(Number(event.target.value))}>
          <option value={0}>Automatic</option>
          {accounts.map((account) => <option key={account.id} value={account.id}>{imapAccountLabel(account)}</option>)}
        </select>
      </div>
      <div>
        <label>Sent Mail folder</label>
        <select value={identity.sent_mailbox_id || 0} onChange={(event) => updateIdentity(identity.id, { sent_mailbox_id: Number(event.target.value) })}>
          <option value={0}>Automatic</option>
          {sentChoices.map((mailbox) => <option key={mailbox.id} value={mailbox.id}>{mailboxChoiceLabel(mailbox, accounts)}</option>)}
        </select>
      </div>
      <div>
        <label>Drafts folder</label>
        <select value={identity.drafts_mailbox_id || 0} onChange={(event) => updateIdentity(identity.id, { drafts_mailbox_id: Number(event.target.value) })}>
          <option value={0}>Automatic</option>
          {draftsChoices.map((mailbox) => <option key={mailbox.id} value={mailbox.id}>{mailboxChoiceLabel(mailbox, accounts)}</option>)}
        </select>
      </div>
    </div>
  );
}

function fallbackThemes(): ThemeDefinition[] {
  return [
    { id: "classic", name: "Classic" },
    { id: "classic_dark", name: "Classic Dark" }
  ];
}

function profileFormForUser(user: User, availableThemes: ThemeDefinition[] = fallbackThemes()) {
  const defaults = searchPresetDefaults(user.search_preset || "balanced");
  const themeIDs = new Set(availableThemes.map((theme) => theme.id));
  return {
    backup_email: user.backup_email || "",
    date_locale: user.date_locale || "",
    date_format: user.date_format || "mdy",
    theme: themeIDs.has(user.theme) ? user.theme : "classic",
    search_preset: ["strict", "balanced", "forgiving"].includes(user.search_preset) ? user.search_preset : defaults.search_preset,
    search_recency_bias: ["none", "light", "normal", "strong"].includes(user.search_recency_bias) ? user.search_recency_bias : defaults.search_recency_bias,
    search_fuzzy: ["off", "balanced", "forgiving"].includes(user.search_fuzzy) ? user.search_fuzzy : defaults.search_fuzzy,
    search_sender_boost: user.search_sender_boost !== false,
    search_sender_history: ["none", "light", "normal", "strong"].includes(user.search_sender_history) ? user.search_sender_history : (user.search_sender_boost === false ? "none" : defaults.search_sender_history),
    search_contact_boost: ["none", "light", "normal", "strong"].includes(user.search_contact_boost) ? user.search_contact_boost : defaults.search_contact_boost,
    search_attachment_weight: ["off", "light", "normal", "strong"].includes(user.search_attachment_weight) ? user.search_attachment_weight : defaults.search_attachment_weight,
    search_compact_splitting: user.search_compact_splitting !== false
  };
}

type SearchChoice = { value: string; label: string };

const searchPresetChoices: SearchChoice[] = [
  { value: "strict", label: "Strict" },
  { value: "balanced", label: "Balanced" },
  { value: "forgiving", label: "Forgiving" }
];

const fuzzyChoices: SearchChoice[] = [
  { value: "off", label: "Off" },
  { value: "balanced", label: "Balanced" },
  { value: "forgiving", label: "Forgiving" }
];

const recencyChoices: SearchChoice[] = [
  { value: "none", label: "None" },
  { value: "light", label: "Light" },
  { value: "normal", label: "Normal" },
  { value: "strong", label: "Strong" }
];

const attachmentWeightChoices: SearchChoice[] = [
  { value: "off", label: "Exclude" },
  { value: "light", label: "Light" },
  { value: "normal", label: "Normal" },
  { value: "strong", label: "Strong" }
];

const boostWeightChoices: SearchChoice[] = [
  { value: "none", label: "None" },
  { value: "light", label: "Light" },
  { value: "normal", label: "Normal" },
  { value: "strong", label: "Strong" }
];

function SearchSliderRow({ title, value, choices, description, onChange }: {
  title: string;
  value: string;
  choices: SearchChoice[];
  description: string;
  onChange: (value: string) => void;
}) {
  const index = Math.max(0, choices.findIndex((choice) => choice.value === value));
  return (
    <div className="search-tuning-row">
      <div className="search-tuning-copy">
        <strong>{title}</strong>
        <small>{description}</small>
      </div>
      <div className="search-slider-control">
        <input
          type="range"
          min={0}
          max={Math.max(0, choices.length - 1)}
          step={1}
          value={index}
          aria-label={title}
          onChange={(event) => onChange(choices[Number(event.target.value)]?.value || choices[0].value)}
        />
        <div className="search-slider-labels">
          {choices.map((choice) => <span className={choice.value === value ? "active" : ""} key={choice.value}>{choice.label}</span>)}
        </div>
      </div>
    </div>
  );
}

function storageEmailDetail(value: unknown): string {
  return `${formatStatCount(value)} emails`;
}

function emptyAccountFormForUser(user: User) {
  const email = user.email || "";
  return {
    ...emptyAccountForm(),
    email,
    label: email,
    username: email,
    mailbox: "*"
  };
}

function emptySMTPFormForUser(user: User) {
  const email = user.email || "";
  return {
    ...emptySMTPForm(),
    label: email,
    username: email
  };
}

function blankMailIdentity(user: User, identities: MailIdentity[] = []): MailIdentity {
  return {
    id: 0,
    contact_id: 0,
    contact_email_id: 0,
    smtp_account_id: 0,
    imap_account_id: 0,
    sent_mailbox_id: 0,
    drafts_mailbox_id: 0,
    email: "",
    display_name: user.name || "",
    signature: "",
    autocrypt_enabled: true,
    is_primary: identities.length === 0
  };
}

function cloneMailIdentity(identity: MailIdentity): MailIdentity {
  return { ...identity, autocrypt_enabled: identity.autocrypt_enabled ?? true };
}

type SettingsRoute = {
  kind: "general" | "profile" | "display" | "storage" | "about" | "mail" | "imap" | "smtp" | "identities" | "preferences" | "swipes" | "search" | "plugins" | "unknown";
  id: number | null;
  isNew: boolean;
};

type FolderSettingsDraft = Pick<Mailbox, "sync_mode" | "role" | "icon" | "show_in_sidebar" | "show_in_all_mail" | "include_in_search">;

// Settings uses real URL subpages for IMAP/SMTP editing so refresh/back keeps
// the selected server instead of returning to the settings index.
function settingsRouteFromPath(path: string): SettingsRoute {
  path = path.length > 1 ? path.replace(/\/+$/, "") : path;
  if (path === "/settings/account" || path === "/settings/account/general") return { kind: "general", id: null, isNew: false };
  if (path === "/settings/account/general/profile") return { kind: "profile", id: null, isNew: false };
  if (path === "/settings/account/general/display") return { kind: "display", id: null, isNew: false };
  if (path === "/settings/account/general/storage") return { kind: "storage", id: null, isNew: false };
  if (path === "/settings/account/general/about") return { kind: "about", id: null, isNew: false };
  if (path === "/settings/account/mail") return { kind: "mail", id: null, isNew: false };
  if (path === "/settings/account/preferences") return { kind: "preferences", id: null, isNew: false };
  if (path === "/settings/account/preferences/swipes") return { kind: "swipes", id: null, isNew: false };
  if (path === "/settings/account/preferences/search") return { kind: "search", id: null, isNew: false };
  if (path === "/settings/account/plugins") return { kind: "plugins", id: null, isNew: false };
  if (path === "/settings/account/mail/identities" || path === "/settings/account/identities") return { kind: "identities", id: null, isNew: false };
  if (path === "/settings/account/mail/imap/new" || path === "/settings/account/imap/new") return { kind: "imap", id: null, isNew: true };
  if (path === "/settings/account/mail/smtp/new" || path === "/settings/account/smtp/new") return { kind: "smtp", id: null, isNew: true };
  const imap = path.match(/^\/settings\/account\/(?:mail\/)?imap\/(\d+)$/);
  if (imap) return { kind: "imap", id: Number(imap[1]), isNew: false };
  const smtp = path.match(/^\/settings\/account\/(?:mail\/)?smtp\/(\d+)$/);
  if (smtp) return { kind: "smtp", id: Number(smtp[1]), isNew: false };
  return { kind: "unknown", id: null, isNew: false };
}

function percentValue(value: number | undefined) {
  const percent = Number(value || 0);
  if (!Number.isFinite(percent)) return 0;
  return Math.max(0, Math.min(100, Math.round(percent)));
}

const syncModeChoices = [
  { value: "auto", label: "Auto", description: "Sync automatically when rolltop refreshes this account." },
  { value: "manual", label: "Manual", description: "Keep the folder available, but sync only when requested." },
  { value: "never", label: "Never", description: "Do not sync this folder." },
  { value: "inherit", label: "Inherit", description: "Use the nearest parent folder sync mode." }
];

const folderRoleChoices = [
  { value: "", label: "Normal" },
  { value: "inbox", label: "Inbox" },
  { value: "sent", label: "Sent" },
  { value: "drafts", label: "Drafts" },
  { value: "trash", label: "Trash" },
  { value: "junk", label: "Spam / Junk" },
  { value: "all", label: "All Mail / Archive" }
];

const uniqueFolderRoles = new Set(folderRoleChoices.map((choice) => choice.value).filter(Boolean));

const folderIconChoices = [
  { value: "folder", label: "Folder" },
  { value: "inbox", label: "Inbox" },
  { value: "archive", label: "Archive" },
  { value: "send", label: "Sent" },
  { value: "draft", label: "Draft" },
  { value: "delete", label: "Trash" },
  { value: "label", label: "Label" },
  { value: "shopping_bag", label: "Purchases" },
  { value: "report", label: "Spam" },
  { value: "star", label: "Star" },
  { value: "bookmark", label: "Bookmark" },
  { value: "flame", label: "Flame" },
  { value: "clock", label: "Clock" },
  { value: "receipt", label: "Receipts" },
  { value: "credit_card", label: "Cards" },
  { value: "briefcase", label: "Work" },
  { value: "bank", label: "Finance" },
  { value: "newspaper", label: "News" },
  { value: "calendar", label: "Calendar" },
  { value: "camera", label: "Photos" },
  { value: "home", label: "Home" },
  { value: "building", label: "Business" },
  { value: "school", label: "School" },
  { value: "travel", label: "Travel" },
  { value: "heart", label: "Personal" },
  { value: "file_text", label: "Docs" },
  { value: "chart", label: "Reports" }
];

const folderVisibilityChoices = [
  { key: "show_in_sidebar", label: "Sidebar" },
  { key: "show_in_all_mail", label: "All Mail" },
  { key: "include_in_search", label: "Search" }
] as const;

const dateLocaleChoices = [
  { value: "", label: "Browser default" },
  { value: "en-US", label: "English (United States)" },
  { value: "en-GB", label: "English (United Kingdom)" },
  { value: "en-CA", label: "English (Canada)" },
  { value: "en-AU", label: "English (Australia)" },
  { value: "fr-FR", label: "French (France)" },
  { value: "fr-CA", label: "French (Canada)" },
  { value: "de-DE", label: "German (Germany)" },
  { value: "es-ES", label: "Spanish (Spain)" },
  { value: "es-MX", label: "Spanish (Mexico)" },
  { value: "it-IT", label: "Italian (Italy)" },
  { value: "pt-BR", label: "Portuguese (Brazil)" },
  { value: "pt-PT", label: "Portuguese (Portugal)" },
  { value: "nl-NL", label: "Dutch (Netherlands)" },
  { value: "sv-SE", label: "Swedish (Sweden)" },
  { value: "da-DK", label: "Danish (Denmark)" },
  { value: "fi-FI", label: "Finnish (Finland)" },
  { value: "nb-NO", label: "Norwegian Bokmal (Norway)" },
  { value: "ja-JP", label: "Japanese (Japan)" },
  { value: "ko-KR", label: "Korean (South Korea)" },
  { value: "zh-CN", label: "Chinese Simplified (China)" },
  { value: "zh-TW", label: "Chinese Traditional (Taiwan)" }
];

function cloneSwipePreferences(preferences: SwipePreferences): SwipePreferences {
  return {
    ...preferences,
    archive_mailboxes: preferences.archive_mailboxes.map((mailbox) => ({ ...mailbox }))
  };
}

function isSwipeArchiveChoice(mailbox: Mailbox): boolean {
  return !["inbox", "sent", "drafts", "trash", "junk"].includes(mailbox.role);
}

function folderCanInherit(mailbox: Mailbox) {
  return folderParentNames(mailbox.name).length > 0;
}

function folderSyncModeChoices(mailbox: Mailbox) {
  return syncModeChoices.filter((choice) => choice.value !== "inherit" || folderCanInherit(mailbox));
}

function folderSettingsDraft(mailbox: Mailbox): FolderSettingsDraft {
  const syncMode = mailbox.sync_mode || (folderCanInherit(mailbox) ? "inherit" : "auto");
  return {
    sync_mode: !folderCanInherit(mailbox) && syncMode === "inherit" ? "auto" : syncMode,
    role: mailbox.role || "",
    icon: mailbox.icon || "folder",
    show_in_sidebar: mailbox.show_in_sidebar,
    show_in_all_mail: mailbox.show_in_all_mail,
    include_in_search: mailbox.include_in_search
  };
}

function folderSyncModeLabel(value: string) {
  return syncModeChoices.find((choice) => choice.value === value)?.label || "Auto";
}

function folderRoleLabel(value: string) {
  return folderRoleChoices.find((choice) => choice.value === value)?.label || "Normal";
}

function folderIconLabel(value: string) {
  return folderIconChoices.find((choice) => choice.value === value)?.label || "Folder";
}

function folderVisibilityLabel(mailbox: Pick<Mailbox, "show_in_sidebar" | "show_in_all_mail" | "include_in_search">) {
  const selected = folderVisibilityChoices.filter((choice) => Boolean(mailbox[choice.key]));
  if (selected.length === folderVisibilityChoices.length) return "Sidebar, All Mail, Search";
  if (selected.length > 0) return selected.map((choice) => choice.label).join(", ");
  return "Hidden";
}

function mailboxNeedsFullTextRefresh(mailbox: Mailbox) {
  if (!mailbox.include_in_search || mailbox.search_index_purged || mailbox.search_index_state_known === false) return false;
  const indexed = mailbox.search_indexed_count;
  const total = mailbox.search_index_total;
  const percent = mailbox.search_index_percent;
  if (typeof indexed === "number" && typeof total === "number") {
    return total > 0 && indexed < total;
  }
  return typeof percent === "number" && percent < 100;
}

function mailboxNeedsLocalMirrorRefresh(mailbox: Mailbox) {
  if (mailbox.remote_uid_next <= 0) return false;
  const local = Math.max(0, mailbox.local_message_count ?? mailbox.message_count);
  return local < Math.max(0, mailbox.remote_message_count);
}

function newerSyncRun(current: SyncRun | null, incoming: SyncRun | null): SyncRun | null {
  if (!current) return incoming;
  if (!incoming) return current;
  if (current.id !== incoming.id) return current.id > incoming.id ? current : incoming;
  const currentUpdated = Date.parse(current.updated_at) || 0;
  const incomingUpdated = Date.parse(incoming.updated_at) || 0;
  if (currentUpdated !== incomingUpdated) return currentUpdated > incomingUpdated ? current : incoming;
  const currentFinished = current.status !== "running";
  const incomingFinished = incoming.status !== "running";
  if (currentFinished !== incomingFinished) return currentFinished ? current : incoming;
  const currentProgress = current.messages_seen + current.messages_stored + current.current_uid;
  const incomingProgress = incoming.messages_seen + incoming.messages_stored + incoming.current_uid;
  return currentProgress > incomingProgress ? current : incoming;
}

function mergeUpdatedSyncRuns(current: SyncRun[], incoming: SyncRun[]): SyncRun[] {
  const byID = new Map(current.map((run) => [run.id, run]));
  incoming.forEach((run) => byID.set(run.id, newerSyncRun(byID.get(run.id) || null, run) || run));
  return Array.from(byID.values())
    .sort((left, right) => {
      const running = Number(right.status === "running") - Number(left.status === "running");
      if (running !== 0) return running;
      const updated = (Date.parse(right.updated_at) || 0) - (Date.parse(left.updated_at) || 0);
      return updated !== 0 ? updated : right.id - left.id;
    })
    .slice(0, 20);
}

function mergeFolderProgress(folder: SyncFolder, progress: FolderProgress, preserveLiveSyncState: boolean): SyncFolder {
  const searchCounts = typeof progress.search_indexed_count === "number"
    ? {
        search_indexed_count: progress.search_indexed_count,
        search_index_total: progress.search_index_total,
        search_index_percent: progress.search_index_percent
      }
    : {};
  if (preserveLiveSyncState) {
    const currentPurged = Boolean(folder.mailbox.search_index_purged);
    const currentKnown = folder.mailbox.search_index_state_known !== false;
    if (currentPurged !== progress.search_index_purged || currentKnown !== progress.search_index_state_known) {
      return folder;
    }
    return {
      ...folder,
      mailbox: { ...folder.mailbox, ...searchCounts }
    };
  }
  return {
    ...folder,
    mailbox: {
      ...folder.mailbox,
      message_count: progress.message_count,
      unread_count: progress.unread_count,
      last_uid: progress.last_uid,
      remote_message_count: progress.remote_message_count,
      remote_unread_count: progress.remote_unread_count,
      remote_uid_next: progress.remote_uid_next,
      sync_percent: progress.sync_percent,
      local_message_count: progress.local_message_count,
      cached_message_count: progress.cached_message_count,
      local_sync_percent: progress.local_sync_percent,
      search_index_purged: progress.search_index_purged,
      search_index_state_known: progress.search_index_state_known,
      ...searchCounts
    },
    is_running: progress.is_running
  };
}

/**
 * SettingsView coordinates account data from /api/account with profile, storage,
 * IMAP, SMTP, identity, and folder-sync editors. Each routed section has an
 * index page and each setting opens in its own detail view.
 */
export function SettingsView({
  csrf,
  user,
  swipePreferences,
  mailboxes,
  latestSyncRun,
  activeSyncRuns,
  syncRunning,
  availableThemes,
  location,
  navigate,
  refreshChrome,
  runtimePlugins,
  reloadRuntimePlugins,
  addToast
}: {
  csrf: string;
  user: User;
  swipePreferences: SwipePreferences;
  mailboxes: Mailbox[];
  latestSyncRun: SyncRun | null;
  activeSyncRuns: SyncRun[];
  syncRunning: boolean;
  availableThemes: ThemeDefinition[];
  location: LocationState;
  navigate: (url: string) => void;
  refreshChrome: () => Promise<Bootstrap | null>;
  runtimePlugins: RuntimePlugins;
  reloadRuntimePlugins: () => Promise<void>;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const route = settingsRouteFromPath(location.path);
  const identitySecurityPlugins: readonly RuntimePlugin[] = runtimePlugins.all;
  const pluginRoutes = accountSettingsRoutes(runtimePlugins);
  const matchedPluginRoute = matchAccountSettingsRoute(runtimePlugins, location.path);
  const [account, setAccount] = useState<Account | null>(null);
  const [imapAccounts, setIMAPAccounts] = useState<Account[]>([]);
  const [smtpAccounts, setSMTPAccounts] = useState<SMTPAccount[]>([]);
  const [identities, setIdentities] = useState<MailIdentity[]>([]);
  const [selectedIdentityID, setSelectedIdentityID] = useState<number | "new" | null>(null);
  const [identityDraft, setIdentityDraft] = useState<MailIdentity>(() => blankMailIdentity(user));
  const [selectedAccountID, setSelectedAccountID] = useState<number | null>(null);
  const [selectedSMTPID, setSelectedSMTPID] = useState<number | null>(null);
  const [runs, setRuns] = useState<SyncRun[]>([]);
  const [folders, setFolders] = useState<SyncFolder[]>([]);
  const [storage, setStorage] = useState<StorageStats>({});
  const [storageLoading, setStorageLoading] = useState(true);
  const [storageError, setStorageError] = useState("");
  const [notice, setNotice] = useState("");
  const [accountNeedsPassword, setAccountNeedsPassword] = useState(false);
  const [form, setForm] = useState(() => emptyAccountForm());
  const [smtpForm, setSMTPForm] = useState(() => emptySMTPForm());
  const [profileForm, setProfileForm] = useState(() => profileFormForUser(user, availableThemes));
  const [swipeDraft, setSwipeDraft] = useState(() => cloneSwipePreferences(swipePreferences));
  const [savingSwipePreferences, setSavingSwipePreferences] = useState(false);
  const swipeDraftDirty = useRef(false);
  const swipeDraftUserID = useRef(user.id);
  const [editingFolderID, setEditingFolderID] = useState<number | null>(null);
  const [folderDraft, setFolderDraft] = useState<FolderSettingsDraft | null>(null);
  const [newFolderName, setNewFolderName] = useState("");
  const [deletingAccountID, setDeletingAccountID] = useState<number | null>(null);
  const [deletingSMTPID, setDeletingSMTPID] = useState<number | null>(null);
  const [creatingFolder, setCreatingFolder] = useState(false);
  const [folderMaintenance, setFolderMaintenance] = useState<{
    mailboxID: number;
    action: "rebuild" | "purge-index" | "purge-references";
  } | null>(null);
  const [accountSearchRebuildID, setAccountSearchRebuildID] = useState<number | null>(null);
  const [folderRunRefreshAccounts, setFolderRunRefreshAccounts] = useState<Set<number>>(() => new Set());
  const [savingIdentity, setSavingIdentity] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState("");
  const locationPathRef = useRef(location.path);
  const selectedAccountIDRef = useRef<number | null>(selectedAccountID);
  const selectedSMTPIDRef = useRef<number | null>(selectedSMTPID);
  const selectedIdentityIDRef = useRef<number | "new" | null>(selectedIdentityID);
  const folderStatusRefreshInFlight = useRef(false);
  const folderLiveStateVersion = useRef(0);
  const folderAccountDataVersion = useRef(0);
  const accountLoadInFlight = useRef(false);
  const folderAccountIDsRef = useRef(new Map<number, number>());

  locationPathRef.current = location.path;
  selectedAccountIDRef.current = selectedAccountID;
  selectedSMTPIDRef.current = selectedSMTPID;
  selectedIdentityIDRef.current = selectedIdentityID;
  folderAccountIDsRef.current = new Map(folders.map((folder) => [folder.mailbox.id, folder.mailbox.account_id]));

  const loadStorage = useCallback(async () => {
    setStorageLoading(true);
    setStorageError("");
    try {
      setStorage(await api.storage());
    } catch (err) {
      setStorageError(messageFromError(err));
    } finally {
      setStorageLoading(false);
    }
  }, []);

  const syncSelectionToRoute = useCallback((path: string, accounts: Account[], smtp: SMTPAccount[], nextIdentities: MailIdentity[]) => {
    const routeForSelection = settingsRouteFromPath(path);
    const currentAccountID = selectedAccountIDRef.current;
    const nextAccountID = routeForSelection.kind === "imap"
      ? routeForSelection.isNew
        ? null
        : routeForSelection.id && accounts.some((item) => item.id === routeForSelection.id)
          ? routeForSelection.id
          : null
      : currentAccountID && accounts.some((item) => item.id === currentAccountID)
        ? currentAccountID
        : accounts[0]?.id || null;
    const nextAccount = accounts.find((item) => item.id === nextAccountID) || null;
    const currentSMTPID = selectedSMTPIDRef.current;
    const nextSMTPID = routeForSelection.kind === "smtp"
      ? routeForSelection.isNew
        ? null
        : routeForSelection.id && smtp.some((item) => item.id === routeForSelection.id)
          ? routeForSelection.id
          : null
      : currentSMTPID && smtp.some((item) => item.id === currentSMTPID)
        ? currentSMTPID
        : smtp[0]?.id || null;
    const nextSMTP = smtp.find((item) => item.id === nextSMTPID) || null;

    selectedAccountIDRef.current = nextAccountID;
    selectedSMTPIDRef.current = nextSMTPID;
    setSelectedAccountID(nextAccountID);
    setSelectedSMTPID(nextSMTPID);
    setAccount(nextAccount);
    setForm(nextAccount ? accountToForm(nextAccount) : emptyAccountFormForUser(user));
    setSMTPForm(nextSMTP ? smtpToForm(nextSMTP) : emptySMTPFormForUser(user));

    const currentIdentityID = selectedIdentityIDRef.current;
    if (currentIdentityID !== "new") {
      const nextIdentity = currentIdentityID
        ? nextIdentities.find((identity) => identity.id === currentIdentityID) || null
        : nextIdentities[0] || null;
      if (nextIdentity) {
        selectedIdentityIDRef.current = nextIdentity.id;
        setSelectedIdentityID(nextIdentity.id);
        setIdentityDraft(cloneMailIdentity(nextIdentity));
      } else {
        selectedIdentityIDRef.current = "new";
        setSelectedIdentityID("new");
        setIdentityDraft(blankMailIdentity(user, nextIdentities));
      }
    }
  }, [user.id, user.email, user.name]);

  // Account data is loaded once on settings entry and explicitly refreshed after
  // mutations. Route changes only select from the cached records, avoiding a
  // second request and loader flash for every settings tab or browser-back step.
  const load = useCallback(async (path = locationPathRef.current) => {
    const loadVersion = folderAccountDataVersion.current + 1;
    folderAccountDataVersion.current = loadVersion;
    accountLoadInFlight.current = true;
    setLoading(true);
    setLoadError("");
    try {
      const data = await api.account();
      if (folderAccountDataVersion.current !== loadVersion) return false;
      const accounts = data.imap_accounts || [];
      const smtp = data.smtp_accounts || [];
      const nextIdentities = data.identities || [];
      setIMAPAccounts(accounts);
      setSMTPAccounts(smtp);
      setIdentities(nextIdentities);
      syncSelectionToRoute(path, accounts, smtp, nextIdentities);
      setRuns(data.sync_runs);
      setFolders(data.sync_folders);
      setNotice(data.notice);
      setAccountNeedsPassword(Boolean(data.account_needs_password));
      if (data.storage) {
        setStorage(data.storage);
        setStorageError("");
        setStorageLoading(false);
      } else {
        void loadStorage();
      }
      setLoaded(true);
      return true;
    } catch (err) {
      if (folderAccountDataVersion.current === loadVersion) setLoadError(messageFromError(err));
      return false;
    } finally {
      if (folderAccountDataVersion.current === loadVersion) {
        accountLoadInFlight.current = false;
        folderAccountDataVersion.current += 1;
        setLoading(false);
      }
    }
  }, [loadStorage, syncSelectionToRoute]);

  // A completed run needs one fresh per-folder history snapshot if SSE was
  // missed. Keep this refresh away from editable account/identity form state.
  const refreshFolderHistory = useCallback(async () => {
    const refreshVersion = folderAccountDataVersion.current + 1;
    folderAccountDataVersion.current = refreshVersion;
    accountLoadInFlight.current = true;
    try {
      const data = await api.account();
      if (folderAccountDataVersion.current !== refreshVersion) return false;
      setRuns((current) => mergeUpdatedSyncRuns(current, data.sync_runs));
      const incomingFolders = new Map(data.sync_folders.map((folder) => [folder.mailbox.id, folder]));
      setFolders((current) => current.map((folder) => {
        const incoming = incomingFolders.get(folder.mailbox.id);
        if (!incoming) return folder;
        const lastRun = newerSyncRun(folder.last_run, incoming.last_run);
        return lastRun === folder.last_run ? folder : { ...folder, last_run: lastRun };
      }));
      return true;
    } catch {
      return false;
    } finally {
      if (folderAccountDataVersion.current === refreshVersion) {
        accountLoadInFlight.current = false;
        folderAccountDataVersion.current += 1;
      }
    }
  }, []);

  const needsFolderStatusRefresh = useMemo(
    () => route.kind === "imap" && !route.isNew && selectedAccountID !== null && (
      folderRunRefreshAccounts.has(selectedAccountID) || folders.some((folder) =>
        folder.mailbox.account_id === selectedAccountID && (
          folder.is_running ||
          mailboxNeedsLocalMirrorRefresh(folder.mailbox) ||
          mailboxNeedsFullTextRefresh(folder.mailbox)
        )
      )
    ),
    [folderRunRefreshAccounts, folders, route.isNew, route.kind, selectedAccountID]
  );

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    const runningAccountIDs = new Set(folders.filter((folder) => folder.is_running).map((folder) => folder.mailbox.account_id));
    if (runningAccountIDs.size === 0) return;
    setFolderRunRefreshAccounts((current) => {
      if (Array.from(runningAccountIDs).every((accountID) => current.has(accountID))) return current;
      const next = new Set(current);
      runningAccountIDs.forEach((accountID) => next.add(accountID));
      return next;
    });
  }, [folders]);

  useEffect(() => {
    if (!loaded || !needsFolderStatusRefresh) return;
    let cancelled = false;
    let refreshTimer: number | undefined;

    function scheduleRefresh() {
      if (!cancelled) refreshTimer = window.setTimeout(refreshStatus, 15_000);
    }

    async function refreshStatus() {
      if (cancelled) return;
      if (folderStatusRefreshInFlight.current || accountLoadInFlight.current) {
        scheduleRefresh();
        return;
      }
      folderStatusRefreshInFlight.current = true;
      try {
        const liveStateVersion = folderLiveStateVersion.current;
        const accountDataVersion = folderAccountDataVersion.current;
        const data = await api.folderProgress();
        if (cancelled || accountDataVersion !== folderAccountDataVersion.current) return;
        const preserveLiveSyncState = liveStateVersion !== folderLiveStateVersion.current;
        const progressByMailbox = new Map(data.folders.map((item) => [item.mailbox_id, item]));
        setFolders((current) => current.map((folder) => {
          const progress = progressByMailbox.get(folder.mailbox.id);
          return progress ? mergeFolderProgress(folder, progress, preserveLiveSyncState) : folder;
        }));
        const selectedID = selectedAccountIDRef.current;
        if (selectedID !== null && folderRunRefreshAccounts.has(selectedID)) {
          const selectedStillRunning = data.folders.some((item) =>
            folderAccountIDsRef.current.get(item.mailbox_id) === selectedID && item.is_running
          );
          if (!selectedStillRunning && await refreshFolderHistory()) {
            setFolderRunRefreshAccounts((current) => {
              if (!current.has(selectedID)) return current;
              const next = new Set(current);
              next.delete(selectedID);
              return next;
            });
          }
        }
      } catch {
        // This is a quiet progress refresh; the initial load still surfaces errors.
      } finally {
        folderStatusRefreshInFlight.current = false;
        scheduleRefresh();
      }
    }

    scheduleRefresh();
    return () => {
      cancelled = true;
      if (refreshTimer !== undefined) window.clearTimeout(refreshTimer);
    };
  }, [folderRunRefreshAccounts, loaded, needsFolderStatusRefresh, refreshFolderHistory]);

  useEffect(() => {
    if (!loaded) return;
    syncSelectionToRoute(location.path, imapAccounts, smtpAccounts, identities);
  }, [identities, imapAccounts, loaded, location.path, smtpAccounts, syncSelectionToRoute]);

  useEffect(() => {
    setProfileForm(profileFormForUser(user, availableThemes));
  }, [
    availableThemes,
    user.date_locale,
    user.date_format,
    user.theme,
    user.search_preset,
    user.search_recency_bias,
    user.search_fuzzy,
    user.search_sender_boost,
    user.search_attachment_weight,
    user.search_compact_splitting
  ]);

  useEffect(() => {
    if (swipeDraftUserID.current !== user.id) {
      swipeDraftUserID.current = user.id;
      swipeDraftDirty.current = false;
    }
    if (!swipeDraftDirty.current) setSwipeDraft(cloneSwipePreferences(swipePreferences));
  }, [swipePreferences, user.id]);

  useEffect(() => {
    folderLiveStateVersion.current += 1;
    setFolders((current) => current.map((folder) => {
      const mailbox = mailboxes.find((item) => item.id === folder.mailbox.id) || folder.mailbox;
      const matchesFolder = (run: SyncRun) => (
        run.account_id === folder.mailbox.account_id &&
        run.current_mailbox.trim().toLowerCase() === folder.mailbox.name.trim().toLowerCase()
      );
      const activeRun = activeSyncRuns.find((run) => run.status === "running" && matchesFolder(run));
      const latestFolderRun = latestSyncRun && matchesFolder(latestSyncRun) ? latestSyncRun : null;
      const latestChanged = Boolean(latestFolderRun && (
        latestFolderRun.id !== folder.last_run?.id ||
        latestFolderRun.status !== folder.last_run?.status ||
        latestFolderRun.updated_at !== folder.last_run?.updated_at
      ));
      const isRunning = Boolean(activeRun) ||
        latestFolderRun?.status === "running" ||
        (!latestChanged && syncRunning && folder.is_running);
      return {
        ...folder,
        mailbox: {
          ...folder.mailbox,
          message_count: mailbox.message_count,
          unread_count: mailbox.unread_count,
          last_uid: mailbox.last_uid,
          remote_message_count: mailbox.remote_message_count,
          remote_unread_count: mailbox.remote_unread_count,
          remote_uid_next: mailbox.remote_uid_next,
          sync_percent: mailbox.sync_percent,
          local_message_count: mailbox.local_message_count,
          local_sync_percent: mailbox.local_sync_percent,
          search_index_purged: mailbox.search_index_purged,
          search_index_state_known: mailbox.search_index_state_known
        },
        is_running: isRunning,
        last_run: activeRun || latestFolderRun || folder.last_run
      };
    }));
    const liveRuns = mergeSyncRuns(activeSyncRuns, latestSyncRun ? [latestSyncRun] : []);
    if (liveRuns.length > 0) {
      setRuns((current) => mergeSyncRuns(liveRuns, current));
    }
  }, [mailboxes, latestSyncRun, activeSyncRuns, syncRunning]);

  // IMAP form edits keep common onboarding assumptions in sync: email seeds the
  // label/username, and same-as-IMAP mirrors credentials into SMTP fields.
  function setField(field: string, value: string | boolean) {
    setForm((current) => {
      const next = { ...current, [field]: value };
      if (field === "email" && typeof value === "string") {
        if (String(current.username || "").trim() === "" || current.username === current.email) {
          next.username = value;
        }
        if (String(current.label || "").trim() === "" || current.label === current.email) {
          next.label = value;
        }
      }
      if (field === "smtp_same_as_imap" && value === true) {
        next.smtp_host = next.host;
        next.smtp_username = next.username;
        next.smtp_password = next.password;
        next.smtp_use_tls = next.use_tls;
      }
      if (next.smtp_same_as_imap && ["host", "username", "password", "use_tls"].includes(field)) {
        next.smtp_host = String(next.host);
        next.smtp_username = String(next.username);
        next.smtp_password = String(next.password);
        next.smtp_use_tls = Boolean(next.use_tls);
      }
      return next;
    });
  }

  function setSMTPField(field: string, value: string | boolean) {
    setSMTPForm((current) => ({ ...current, [field]: value }));
  }

  function selectIMAP(next: Account) {
    setSelectedAccountID(next.id);
    setAccount(next);
    setForm(accountToForm(next));
  }

  function newIMAPAccount() {
    setSelectedAccountID(null);
    setAccount(null);
    setForm(emptyAccountFormForUser(user));
    navigate("/settings/account/mail/imap/new");
  }

  function selectSMTP(next: SMTPAccount) {
    setSelectedSMTPID(next.id);
    setSMTPForm(smtpToForm(next));
  }

  function newSMTPAccount() {
    setSelectedSMTPID(null);
    setSMTPForm(emptySMTPFormForUser(user));
    navigate("/settings/account/mail/smtp/new");
  }

  function chooseIdentity(identity: MailIdentity) {
    setSelectedIdentityID(identity.id);
    setIdentityDraft(cloneMailIdentity(identity));
  }

  function newIdentity() {
    setSelectedIdentityID("new");
    setIdentityDraft(blankMailIdentity(user, identities));
    navigate("/settings/account/mail/identities");
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    try {
      const data = await api.saveIMAPAccount(csrf, {
        id: selectedAccountID || 0,
        email: form.email,
        label: form.label,
        host: form.host,
        port: Number(form.port),
        username: form.username,
        password: form.password,
        use_tls: form.use_tls,
        smtp_host: form.smtp_host,
        smtp_port: Number(form.smtp_port),
        smtp_username: form.smtp_username,
        smtp_password: form.smtp_password,
        smtp_use_tls: form.smtp_use_tls,
        smtp_same_as_imap: form.smtp_same_as_imap,
        mailbox: form.mailbox,
        sync_interval_minutes: Number(form.sync_interval_minutes)
      });
      addToast("IMAP server saved.");
      setSelectedAccountID(data.account.id);
      const nextPath = `/settings/account/mail/imap/${data.account.id}`;
      navigate(nextPath);
      await load(nextPath);
      await refreshChrome();
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function saveSMTP(event: FormEvent) {
    event.preventDefault();
    try {
      const data = await api.saveSMTPAccount(csrf, {
        id: selectedSMTPID || 0,
        label: smtpForm.label,
        host: smtpForm.host,
        port: Number(smtpForm.port),
        username: smtpForm.username,
        password: smtpForm.password,
        use_tls: smtpForm.use_tls
      });
      addToast("SMTP server saved.");
      setSelectedSMTPID(data.smtp_account.id);
      const nextPath = `/settings/account/mail/smtp/${data.smtp_account.id}`;
      navigate(nextPath);
      await load(nextPath);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function deleteSelectedSMTPAccount() {
    if (!selectedSMTP || deletingSMTPID) return;
    const expected = smtpDeleteConfirmationName(selectedSMTP);
    setDeletingSMTPID(selectedSMTP.id);
    try {
      const typed = window.prompt([
        `Remove the SMTP server "${expected}" from rolltop?`,
        "",
        "This does not delete messages or Me identities.",
        "Identities using this SMTP server will be set back to Default.",
        "",
        `Type ${expected} to confirm.`
      ].join("\n"));
      if (typed === null) return;
      if (typed.trim() !== expected) {
        addToast("SMTP server name did not match. Nothing was removed.", "error");
        return;
      }
      await api.deleteSMTPAccount(csrf, selectedSMTP.id);
      addToast(`Removed ${expected}. Identities using it now use Default.`);
      setSMTPAccounts((current) => current.filter((item) => item.id !== selectedSMTP.id));
      setIdentities((current) => current.map((identity) => identity.smtp_account_id === selectedSMTP.id ? { ...identity, smtp_account_id: 0 } : identity));
      setIdentityDraft((current) => current.smtp_account_id === selectedSMTP.id ? { ...current, smtp_account_id: 0 } : current);
      setSelectedSMTPID(null);
      setSMTPForm(emptySMTPFormForUser(user));
      navigate("/settings/account/mail");
      await refreshChrome();
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setDeletingSMTPID(null);
    }
  }


  async function saveIdentity(identity: MailIdentity) {
    setSavingIdentity(true);
    try {
      const data = await api.saveMailIdentity(csrf, identity);
      setIdentities(data.identities);
      setSelectedIdentityID(data.identity.id);
      setIdentityDraft(cloneMailIdentity(data.identity));
      addToast(`${data.identity.email} identity saved.`);
      await refreshChrome();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setSavingIdentity(false);
    }
  }

  function updateIdentityDraft(patch: Partial<MailIdentity>) {
    setIdentityDraft((current) => ({ ...current, ...patch }));
  }

  function markIdentityAutocryptEnabled(identityID: number) {
    setIdentities((current) => current.map((identity) => identity.id === identityID ? { ...identity, autocrypt_enabled: true } : identity));
    setIdentityDraft((current) => current.id === identityID ? { ...current, autocrypt_enabled: true } : current);
  }

  async function saveProfile(event: FormEvent) {
    event.preventDefault();
    try {
      await api.saveProfile(csrf, profileForm);
      addToast("Preferences saved.");
      await refreshChrome();
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function saveSwipeSettings(event: FormEvent) {
    event.preventDefault();
    const archiveRequired = swipeDraft.left_action === "archive" || swipeDraft.right_action === "archive";
    const trashRequired = swipeDraft.left_action === "trash" || swipeDraft.right_action === "trash";
    if (archiveRequired && imapAccounts.length === 0) {
      addToast("Add an IMAP server before choosing an archive swipe action.", "error");
      return;
    }
    if (trashRequired && imapAccounts.length === 0) {
      addToast("Add an IMAP server before choosing a trash swipe action.", "error");
      return;
    }
    const missingTrashAccount = trashRequired
      ? imapAccounts.find((account) => !trashMailboxForAccount(mailboxes, account.id))
      : undefined;
    if (missingTrashAccount) {
      addToast(`Choose a Trash folder for ${imapAccountLabel(missingTrashAccount)}.`, "error");
      return;
    }
    const archiveByAccount = new Map(swipeDraft.archive_mailboxes.map((item) => [item.account_id, item.mailbox_id]));
    const missingAccount = archiveRequired
      ? imapAccounts.find((account) => {
          const mailboxID = archiveByAccount.get(account.id);
          return !mailboxes.some((mailbox) => mailbox.id === mailboxID && mailbox.account_id === account.id && isSwipeArchiveChoice(mailbox));
        })
      : undefined;
    if (missingAccount) {
      addToast(`Choose an archive folder for ${imapAccountLabel(missingAccount)}.`, "error");
      return;
    }
    const accountIDs = new Set(imapAccounts.map((item) => item.id));
    const preferences: SwipePreferences = {
      ...swipeDraft,
      archive_mailboxes: swipeDraft.archive_mailboxes.filter((item) =>
        accountIDs.has(item.account_id) && mailboxes.some((mailbox) =>
          mailbox.id === item.mailbox_id && mailbox.account_id === item.account_id && isSwipeArchiveChoice(mailbox)
        )
      )
    };
    setSavingSwipePreferences(true);
    try {
      const saved = await api.saveSwipePreferences(csrf, preferences);
      swipeDraftDirty.current = false;
      setSwipeDraft(cloneSwipePreferences(saved.swipe_preferences));
      addToast("Swipe actions saved.");
      await refreshChrome();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setSavingSwipePreferences(false);
    }
  }

  async function syncNow() {
    try {
      await api.syncAccount(csrf);
      addToast("Sync started.");
      await refreshChrome();
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  function rebuildAccountSearchIndexConfirmMessage() {
    const searchable = selectedFolders.filter((folder) => folder.mailbox.include_in_search);
    const localCount = searchable.reduce((total, folder) => total + Math.max(0, folder.mailbox.local_message_count ?? folder.mailbox.message_count), 0);
    return [
      `Rebuild full-text indexes for ${selectedAccountLabel}?`,
      "",
      `This replaces the local full-text documents for ${searchable.length.toLocaleString()} ${searchable.length === 1 ? "folder" : "folders"} and ${localCount.toLocaleString()} mirrored ${localCount === 1 ? "message" : "messages"}.`,
      "If a mirrored message's raw data is no longer cached, Rolltop may fetch it from IMAP.",
      "",
      "This does not change or delete messages on the IMAP server."
    ].join("\n");
  }

  async function rebuildAccountSearchIndexes() {
    if (!account || accountSearchRebuildID || folderMaintenance || !window.confirm(rebuildAccountSearchIndexConfirmMessage())) return;
    setAccountSearchRebuildID(account.id);
    try {
      await api.rebuildIMAPAccountSearchIndex(csrf, account.id);
      addToast(`${selectedAccountLabel} full-text index rebuild started.`);
      await refreshChrome();
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setAccountSearchRebuildID(null);
    }
  }

  async function deleteSelectedIMAPAccount() {
    if (!account || deletingAccountID) return;
    setDeletingAccountID(account.id);
    try {
      const estimate = await api.imapAccountPurgeEstimate(account.id);
      const expected = estimate.account_name;
      const typed = window.prompt(imapDeletePrompt(estimate));
      if (typed === null) return;
      if (typed.trim() !== expected) {
        addToast("IMAP server name did not match. Nothing was removed.", "error");
        return;
      }
      await api.deleteIMAPAccount(csrf, account.id, typed.trim());
      addToast(`Removing ${expected} locally. Remote IMAP mail is untouched.`);
      setIMAPAccounts((current) => current.filter((item) => item.id !== account.id));
      setFolders((current) => current.filter((folder) => folder.mailbox.account_id !== account.id));
      setIdentities((current) => current.map((identity) => identity.imap_account_id === account.id ? { ...identity, imap_account_id: 0, sent_mailbox_id: 0, drafts_mailbox_id: 0 } : identity));
      setIdentityDraft((current) => current.imap_account_id === account.id ? { ...current, imap_account_id: 0, sent_mailbox_id: 0, drafts_mailbox_id: 0 } : current);
      setSelectedAccountID(null);
      setAccount(null);
      navigate("/settings/account/mail");
      await refreshChrome();
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setDeletingAccountID(null);
    }
  }

  // Folder settings are saved as whole visibility/role/icon/sync-mode snapshots
  // so each small UI control can optimistically patch the same mailbox object.
  async function saveFolderSettings(folder: SyncFolder, patch: Partial<Mailbox>): Promise<boolean> {
    const next = { ...folder.mailbox, ...patch };
    const enablingSearch = !folder.mailbox.include_in_search && next.include_in_search;
    try {
      const result = await api.saveFolderSettings(csrf, folder.mailbox.id, {
        sync_mode: next.sync_mode,
        role: next.role || "",
        icon: next.icon || "folder",
        show_in_sidebar: next.show_in_sidebar,
        show_in_all_mail: next.show_in_all_mail,
        include_in_search: next.include_in_search
      });
      const optimisticMailbox = enablingSearch ? {
        ...next,
        search_index_purged: false,
        search_index_state_known: false,
        search_indexed_count: undefined,
        search_index_total: undefined,
        search_index_percent: undefined
      } : next;
      setFolders((current) => current.map((item) => item.mailbox.id === folder.mailbox.id
        ? { ...item, mailbox: optimisticMailbox, is_running: result.queued || item.is_running }
        : item));
      if (result.queued) {
        setFolderRunRefreshAccounts((current) => {
          const nextAccounts = new Set(current);
          nextAccounts.add(folder.mailbox.account_id);
          return nextAccounts;
        });
      }
      addToast(`${folder.mailbox.name} updated.`);
      await refreshChrome();
      if (result.queued) {
        try {
          const liveStateVersion = folderLiveStateVersion.current;
          const progress = await api.folderProgress();
          const preserveLiveSyncState = liveStateVersion !== folderLiveStateVersion.current;
          const progressByMailbox = new Map(progress.folders.map((item) => [item.mailbox_id, item]));
          setFolders((current) => current.map((item) => {
            const snapshot = progressByMailbox.get(item.mailbox.id);
            return snapshot ? mergeFolderProgress(item, snapshot, preserveLiveSyncState) : item;
          }));
        } catch {
          // The queued run and regular compact poll still provide recovery.
        }
      }
      return true;
    } catch (err) {
      addToast(messageFromError(err), "error");
      return false;
    }
  }

  function openFolderSettings(folder: SyncFolder) {
    setEditingFolderID(folder.mailbox.id);
    setFolderDraft(folderSettingsDraft(folder.mailbox));
  }

  function closeFolderSettings() {
    setEditingFolderID(null);
    setFolderDraft(null);
  }

  function updateFolderDraft(patch: Partial<FolderSettingsDraft>) {
    setFolderDraft((current) => current ? { ...current, ...patch } : current);
  }

  function purgeSearchIndexConfirmMessage(folder: SyncFolder) {
    return [
      `Purge the full-text index for ${folder.mailbox.name}?`,
      "",
      "This removes only rolltop's local full-text search documents for this folder.",
      "It does not delete local message references or messages from your IMAP server.",
      "Use Rebuild full-text index when you want to restore the local full-text documents."
    ].join("\n");
  }

  function rebuildSearchIndexConfirmMessage(folder: SyncFolder) {
    const localCount = Math.max(0, folder.mailbox.local_message_count ?? folder.mailbox.message_count);
    const remoteCount = Math.max(0, folder.mailbox.remote_message_count);
    const remoteStatusAvailable = folder.mailbox.remote_uid_next > 0;
    const lines = [
      `Rebuild the full-text index for ${folder.mailbox.name}?`,
      "",
      `This rebuild covers the ${localCount.toLocaleString()} ${localCount === 1 ? "message" : "messages"} currently mirrored locally.`,
      "If a mirrored message's raw data is no longer cached, Rolltop may fetch it from IMAP."
    ];
    if (remoteStatusAvailable && localCount < remoteCount) {
      lines.push(
        "",
        `${(remoteCount - localCount).toLocaleString()} of ${remoteCount.toLocaleString()} remote messages are not mirrored locally and cannot be included. Sync this folder to restore the remainder.`
      );
    }
    lines.push("", "This does not change or delete messages on the IMAP server.");
    return lines.join("\n");
  }

  function purgeLocalReferencesConfirmMessage(folder: SyncFolder) {
    return [
      `Purge local references and the full-text index for ${folder.mailbox.name}?`,
      "",
      "This removes rolltop's local message references, raw-message cache, and full-text search documents for this folder.",
      "It does not delete messages from your IMAP server.",
      "The next sync will refetch this folder from IMAP and rebuild the local full-text index."
    ].join("\n");
  }

  function folderRoleConflict(folder: SyncFolder, role: string) {
    if (!uniqueFolderRoles.has(role)) return null;
    return folders.find((item) =>
      item.mailbox.account_id === folder.mailbox.account_id &&
      item.mailbox.id !== folder.mailbox.id &&
      (item.mailbox.role || "") === role
    ) || null;
  }

  async function saveEditingFolder(event: FormEvent) {
    event.preventDefault();
    if (!editingFolderID || !folderDraft) return;
    const folder = folderMap.get(editingFolderID);
    if (!folder) {
      closeFolderSettings();
      return;
    }
    const nextDraft = { ...folderDraft };
    if (!folderCanInherit(folder.mailbox) && nextDraft.sync_mode === "inherit") {
      nextDraft.sync_mode = "auto";
    }
    const conflict = folderRoleConflict(folder, nextDraft.role || "");
    if (conflict) {
      addToast(`${folderRoleLabel(nextDraft.role || "")} is already assigned to ${conflict.mailbox.name}.`, "error");
      return;
    }
    const saved = await saveFolderSettings(folder, nextDraft);
    if (saved) closeFolderSettings();
  }

  async function syncFolder(folder: SyncFolder) {
    try {
      await api.syncFolder(csrf, folder.mailbox.id);
      addToast(`${folder.mailbox.name} sync started.`);
      await refreshChrome();
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function createFolder(event: FormEvent) {
    event.preventDefault();
    if (!selectedAccountID || creatingFolder) return;
    const name = newFolderName.trim();
    if (!name) {
      addToast("Folder name is required.", "error");
      return;
    }
    setCreatingFolder(true);
    try {
      const data = await api.createIMAPFolder(csrf, selectedAccountID, name);
      setNewFolderName("");
      addToast(`${data.mailbox.name} created on the IMAP server.`);
      await load();
      await refreshChrome();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setCreatingFolder(false);
    }
  }

  async function purgeFolderSearchIndex(folder: SyncFolder) {
    if (folderMaintenance || !window.confirm(purgeSearchIndexConfirmMessage(folder))) return;
    setFolderMaintenance({ mailboxID: folder.mailbox.id, action: "purge-index" });
    try {
      await api.purgeFolderSearchIndex(csrf, folder.mailbox.id);
      addToast(`${folder.mailbox.name} full-text index purge started.`);
      await refreshChrome();
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setFolderMaintenance(null);
    }
  }

  async function rebuildFolderSearchIndex(folder: SyncFolder) {
    if (folderMaintenance || !window.confirm(rebuildSearchIndexConfirmMessage(folder))) return;
    setFolderMaintenance({ mailboxID: folder.mailbox.id, action: "rebuild" });
    try {
      await api.rebuildFolderSearchIndex(csrf, folder.mailbox.id);
      addToast(`${folder.mailbox.name} full-text index rebuild started.`);
      await refreshChrome();
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setFolderMaintenance(null);
    }
  }

  async function purgeFolderLocalReferences(folder: SyncFolder) {
    if (folderMaintenance || !window.confirm(purgeLocalReferencesConfirmMessage(folder))) return;
    setFolderMaintenance({ mailboxID: folder.mailbox.id, action: "purge-references" });
    try {
      await api.purgeFolderLocalReferences(csrf, folder.mailbox.id);
      addToast(`${folder.mailbox.name} local references purge started.`);
      await refreshChrome();
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setFolderMaintenance(null);
    }
  }

  const allFolderMailboxes = useMemo(() => folders.map((folder) => folder.mailbox), [folders]);
  const selectedFolders = useMemo(
    () => selectedAccountID ? folders.filter((folder) => folder.mailbox.account_id === selectedAccountID) : folders,
    [folders, selectedAccountID]
  );
  const selectedFolderMailboxes = useMemo(() => selectedFolders.map((folder) => folder.mailbox), [selectedFolders]);
  const folderMap = useMemo(() => new Map(selectedFolders.map((folder) => [folder.mailbox.id, folder])), [selectedFolders]);
  const folderNodes = useMemo(() => folderTree(selectedFolderMailboxes, { includeHidden: true }), [selectedFolderMailboxes]);
  const selectedAccountLabel = account ? (account.label || account.email) : route.kind === "imap" && route.isNew ? "New IMAP server" : "IMAP server";
  const selectedSMTP = smtpAccounts.find((item) => item.id === selectedSMTPID) || null;
  const identitiesBySMTP = useMemo(() => {
    const byServer = new Map<number, MailIdentity[]>();
    identities.forEach((identity) => {
      if (!identity.smtp_account_id) return;
      const existing = byServer.get(identity.smtp_account_id) || [];
      existing.push(identity);
      byServer.set(identity.smtp_account_id, existing);
    });
    return byServer;
  }, [identities]);
  const selectedSMTPLabel = selectedSMTP ? (selectedSMTP.label || selectedSMTP.host) : selectedSMTPID ? "SMTP server" : "New SMTP server";

  useEffect(() => {
    if (editingFolderID && !folderMap.has(editingFolderID)) {
      setEditingFolderID(null);
      setFolderDraft(null);
    }
  }, [editingFolderID, folderMap]);

  useEffect(() => {
    if (!editingFolderID) return;
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") closeFolderSettings();
    }
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [editingFolderID]);

  // Folder settings render from the same tree model as the sidebar, with
  // maintenance actions and editable options separated from the read-only row.
  function renderFolderItems(nodes: FolderNode[], depth = 0): ReactNode[] {
    return nodes.flatMap((node) => {
      const folder = folderMap.get(node.mailbox.id);
      if (!folder) return [];
      const localPercent = percentValue(folder.mailbox.local_sync_percent ?? folder.mailbox.sync_percent);
      const localCount = Math.max(0, folder.mailbox.local_message_count ?? folder.mailbox.message_count);
      const cachedCount = Math.max(0, folder.mailbox.cached_message_count ?? 0);
      const remoteCount = Math.max(0, folder.mailbox.remote_message_count);
      const remoteStatusAvailable = folder.mailbox.remote_uid_next > 0;
      const localLabel = remoteStatusAvailable
		? `Headers ${localCount.toLocaleString()}/${remoteCount.toLocaleString()}`
		: `Headers ${localCount.toLocaleString()} mirrored`;
      const missingLocalCount = Math.max(0, remoteCount - localCount);
		let localProgressTitle = `Headers are stored locally for ${localCount.toLocaleString()} messages; the latest remote IMAP count is ${remoteCount.toLocaleString()}.`;
		if (!remoteStatusAvailable) {
			localProgressTitle = `Headers are stored locally for ${localCount.toLocaleString()} messages. The remote IMAP count is not available yet.`;
		} else if (missingLocalCount > 0) {
			localProgressTitle = `Headers are stored locally for ${localCount.toLocaleString()} messages; the latest remote IMAP count is ${remoteCount.toLocaleString()}. Sync this folder to restore ${missingLocalCount.toLocaleString()} missing headers.`;
      }
      const searchIndexedCount = folder.mailbox.search_indexed_count;
      const searchTotalCount = folder.mailbox.search_index_total;
      const searchIndexPurged = Boolean(folder.mailbox.search_index_purged);
      const searchIndexKnown = folder.mailbox.search_index_state_known !== false;
      const hasSearchCounts = searchIndexKnown && typeof searchIndexedCount === "number" && typeof searchTotalCount === "number";
      const searchCounts = hasSearchCounts
        ? `${searchIndexedCount.toLocaleString()}/${searchTotalCount.toLocaleString()}`
        : "count unavailable";
      const searchPercent = hasSearchCounts
        ? percentValue(typeof folder.mailbox.search_index_percent === "number"
          ? folder.mailbox.search_index_percent
          : searchTotalCount > 0 ? Math.floor((searchIndexedCount * 100) / searchTotalCount) : 0)
        : null;
      const searchLabel = !folder.mailbox.include_in_search
		? "Full Text Search off"
		: searchIndexPurged
		  ? "Full Text Search purged"
		  : !searchIndexKnown
			? "Full Text Search not audited"
			: searchPercent === null
			  ? "Full Text Search unavailable"
			  : `Full Text Search ${searchCounts}`;
      const searchProgressTitle = folder.mailbox.include_in_search
        ? searchIndexPurged
          ? "This folder's local full-text documents were deliberately purged. Use Rebuild full-text index to restore them."
          : !searchIndexKnown
            ? "Current full-text coverage has not been audited since this Rolltop upgrade. It remains usable; use Rebuild full-text index to replace and verify it."
            : searchPercent === null
              ? "Full-text search coverage is unavailable. Header and preview search may still work."
              : `${formatStatCount(searchIndexedCount)} of ${formatStatCount(searchTotalCount)} locally mirrored messages completed a full-text indexing commit (${searchPercent}%).`
        : "Full-message search is disabled for this folder.";
      const cachePercent = localCount > 0 ? percentValue(Math.floor((cachedCount * 100) / localCount)) : 0;
		const cacheLabel = `Local Cache ${cachedCount.toLocaleString()}/${localCount.toLocaleString()}`;
		const cacheProgressTitle = `Local Cache retains the original RFC 822 message for ${cachedCount.toLocaleString()} of ${localCount.toLocaleString()} locally stored headers. This cache is bounded by the retention setting.`;
      const currentRole = folder.mailbox.role || "";
      const currentIcon = folder.mailbox.icon || "folder";
      const syncLabel = folderSyncModeLabel(folder.mailbox.sync_mode || "inherit");
      const effectiveSyncMode = effectiveMailboxSyncMode(folder.mailbox, selectedFolderMailboxes);
      const canShowSyncAction = effectiveSyncMode !== "never";
      const roleLabel = folderRoleLabel(currentRole);
      const iconLabel = folderIconLabel(currentIcon);
      const visibilityLabel = folderVisibilityLabel(folder.mailbox);
      const rows: ReactNode[] = [
        <div className="folder-sync-row" key={folder.mailbox.id}>
          <div className="folder-sync-summary">
            <div className="folder-sync-name" style={depth > 0 ? { paddingLeft: `${depth * 18}px` } : undefined}>
              <Icon name={currentIcon} />
              <div>
                <strong>{node.label}</strong>
                <small>{folder.mailbox.account_email || "Mail account"} · {folder.mailbox.name}</small>
              </div>
            </div>
            <div className="folder-sync-status">
              <div className="folder-index-meters">
                <div className="sync-percent" aria-label={localProgressTitle} title={localProgressTitle}>
                  <div><span style={{ width: `${localPercent}%` }} /></div>
                  <small>{localLabel}</small>
                </div>
                <div className="sync-percent" aria-label={cacheProgressTitle} title={cacheProgressTitle}>
                  <div><span style={{ width: `${cachePercent}%` }} /></div>
                  <small>{cacheLabel}</small>
                </div>
                <div className="sync-percent" aria-label={searchProgressTitle} title={searchProgressTitle}>
                  <div><span style={{ width: `${searchPercent ?? 0}%` }} /></div>
                  <small>{searchLabel}</small>
                </div>
              </div>
              <div className="folder-sync-counts">
                <span><strong>{folder.mailbox.message_count.toLocaleString()}</strong> messages</span>
                <span><strong>{folder.mailbox.unread_count.toLocaleString()}</strong> unread</span>
              </div>
            </div>
            <div className="folder-sync-run">
              <span className="settings-field-label">Last run</span>
              <strong>{folder.is_running ? "Running" : folder.last_run ? folder.last_run.status : "Never"}</strong>
              <small>{folder.last_run ? displayDateTime(folder.last_run.updated_at, user) : "No sync activity"}</small>
            </div>
            <div className="folder-sync-settings-summary" aria-label={`${node.label} folder settings`}>
              <span title={`Sync mode: ${syncLabel}`}>Sync {syncLabel}</span>
              <span title={`Folder role: ${roleLabel}`}>{roleLabel}</span>
              <span title={`Sidebar icon: ${iconLabel}`}><Icon name={currentIcon} />{iconLabel}</span>
              <span title={`Visible in: ${visibilityLabel}`}>{visibilityLabel}</span>
            </div>
            <div className="folder-actions" aria-label={`${node.label} actions`}>
              {canShowSyncAction ? (
                <button
                  className="folder-icon-action"
                  type="button"
                  disabled={!folder.can_sync_now}
                  onClick={() => syncFolder(folder)}
                  title={folder.can_sync_now ? `Sync ${node.label} now` : `${node.label} is already syncing`}
                  aria-label={`Sync ${node.label} now`}
                >
                  <Icon name="sync" />
                </button>
              ) : null}
              <button
                className="folder-icon-action"
                type="button"
                onClick={() => openFolderSettings(folder)}
                title={`Edit settings for ${node.label}`}
                aria-label={`Edit settings for ${node.label}`}
              >
                <Icon name="edit" />
              </button>
            </div>
          </div>
        </div>
      ];
      return rows.concat(renderFolderItems(node.children, depth + 1));
    });
  }

  function renderIdentitySettings() {
    const selectedTitle = selectedIdentityID === "new" ? "New identity" : identityDraft.display_name || identityDraft.email || "Identity";
    return (
      <section className="contacts-shell identity-settings-shell">
        <aside className="contacts-list">
          <div className="contacts-list-items">
            {identities.length === 0 ? <div className="muted">No identities yet.</div> : null}
            {identities.map((identity) => (
              <button
                type="button"
                className={`contact-row ${identity.id === selectedIdentityID ? "active" : ""}`}
                key={identity.id}
                onClick={() => chooseIdentity(identity)}
              >
                <span className="server-row-icon"><Icon name="group" /></span>
                <span>
                  <strong>{identity.display_name || identity.email}</strong>
                  <small>{identity.email}</small>
                </span>
                {identity.is_primary ? <em>Primary</em> : null}
              </button>
            ))}
          </div>
        </aside>
        <form className="contact-editor identity-editor" onSubmit={(event) => { event.preventDefault(); void saveIdentity(identityDraft); }}>
          <div className="panel-headline">
            <div>
              <h2>{selectedTitle}</h2>
              <div className="muted">SMTP, IMAP, Sent/Drafts folders, and signature.</div>
            </div>
          </div>
          <div className="identity-main">
            <Field label="Display name" value={identityDraft.display_name} required onChange={(value) => updateIdentityDraft({ display_name: value })} />
            <div>
              <label>Email</label>
              <input
                type="email"
                value={identityDraft.email}
                readOnly={identityDraft.id > 0}
                required
                onChange={(event) => updateIdentityDraft({ email: event.target.value })}
              />
            </div>
            <div>
              <label>Outbound SMTP server</label>
              <select value={identityDraft.smtp_account_id || 0} onChange={(event) => updateIdentityDraft({ smtp_account_id: Number(event.target.value) })}>
                <option value={0}>Default</option>
                {smtpAccounts.map((smtp) => <option value={smtp.id} key={smtp.id}>{smtpAccountLabel(smtp)}</option>)}
              </select>
            </div>
            <label className="identity-primary"><input type="checkbox" checked={identityDraft.is_primary} onChange={(event) => updateIdentityDraft({ is_primary: event.target.checked })} /> Primary</label>
          </div>
          <IdentityMailboxFields identity={identityDraft} accounts={imapAccounts} smtpAccounts={smtpAccounts} mailboxes={allFolderMailboxes} updateIdentity={(_, patch) => updateIdentityDraft(patch)} />
          {identitySecuritySettings(identitySecurityPlugins, {
            csrf,
            user,
            identities,
            identityDraft,
            updateIdentityDraft,
            markIdentitySecurityReady: markIdentityAutocryptEnabled,
            addToast
          })}
          <div>
            <label>Signature</label>
            <RichSignatureEditor value={identityDraft.signature} onChange={(value) => updateIdentityDraft({ signature: value })} />
          </div>
          <div className="contact-savebar">
            <button disabled={savingIdentity}>{savingIdentity ? "Saving..." : "Save identity"}</button>
          </div>
        </form>
      </section>
    );
  }

  function renderProfileSettings() {
    const displayName = user.name || user.email;
    return (
      <section className="panel profile-settings">
        <div className="muted settings-section-note">Signed in as {displayName}</div>
        <div className="profile-account-summary">
          <span className="settings-field-label">Account</span>
          <strong>{displayName}</strong>
          <small>{user.email}</small>
        </div>
        <form className="profile-backup-email" onSubmit={saveProfile}>
          <div>
            <label>Backup email</label>
            <input
              type="email"
              value={profileForm.backup_email}
              placeholder="Used only for password resets"
              onChange={(event) => setProfileForm((current) => ({ ...current, backup_email: event.target.value }))}
            />
          </div>
          <button className="secondary" type="submit">Save backup email</button>
        </form>
      </section>
    );
  }

  function renderDisplaySettings() {
    return (
      <form className="panel display-settings" onSubmit={saveProfile}>
        <div className="settings-columns display-settings-grid">
          <section>
            <h3>Date localization</h3>
            <label>Locale</label>
            <select value={profileForm.date_locale} onChange={(event) => setProfileForm((current) => ({ ...current, date_locale: event.target.value }))}>
              {(profileForm.date_locale && !dateLocaleChoices.some((choice) => choice.value === profileForm.date_locale)
                ? [...dateLocaleChoices, { value: profileForm.date_locale, label: `${profileForm.date_locale} (saved custom)` }]
                : dateLocaleChoices
              ).map((choice) => (
                <option value={choice.value} key={choice.value || "browser-default"}>{choice.label}</option>
              ))}
            </select>
          </section>
          <section>
            <h3>Date format</h3>
            <label>Date style</label>
            <select value={profileForm.date_format} onChange={(event) => setProfileForm((current) => ({ ...current, date_format: event.target.value }))}>
              <option value="mdy">MM/DD/YY</option>
              <option value="dmy">DD/MM/YY</option>
              <option value="ymd">YY/MM/DD</option>
              <option value="locale">Locale default</option>
            </select>
          </section>
          <section>
            <h3>Theme</h3>
            <label>Interface style</label>
            <select value={profileForm.theme} onChange={(event) => setProfileForm((current) => ({ ...current, theme: event.target.value }))}>
              {(availableThemes.length > 0 ? availableThemes : fallbackThemes()).map((theme) => (
                <option value={theme.id} key={theme.id}>{theme.name}</option>
              ))}
            </select>
          </section>
          <section>
            <h3>Preview</h3>
            <div className="date-preview">
              <span>Recent mail</span>
              <strong>{displayTime(new Date().toISOString(), profileForm)}</strong>
              <span>Older mail</span>
              <strong>{displayTime(new Date(Date.now() - 400 * 24 * 60 * 60 * 1000).toISOString(), profileForm)}</strong>
            </div>
          </section>
        </div>
        <div className="actions"><button>Save display</button></div>
      </form>
    );
  }

  function renderSwipeSettings() {
    const archiveRequired = swipeDraft.left_action === "archive" || swipeDraft.right_action === "archive";
    const archiveUnavailable = archiveRequired && imapAccounts.length === 0;
    const trashRequired = swipeDraft.left_action === "trash" || swipeDraft.right_action === "trash";
    const trashUnavailable = trashRequired && imapAccounts.length === 0;
    const missingTrashAccounts = trashRequired
      ? imapAccounts.filter((account) => !trashMailboxForAccount(mailboxes, account.id))
      : [];
    const archiveByAccount = new Map(swipeDraft.archive_mailboxes.map((item) => [item.account_id, item.mailbox_id]));
    const archiveChoices = (accountID: number) => mailboxes
      .filter((mailbox) => mailbox.account_id === accountID && isSwipeArchiveChoice(mailbox))
      .sort((left, right) => left.name.localeCompare(right.name));
    const missingArchiveAccounts = archiveRequired
      ? imapAccounts.filter((account) => {
          const selectedID = archiveByAccount.get(account.id);
          return !selectedID || !archiveChoices(account.id).some((mailbox) => mailbox.id === selectedID);
        })
      : [];

    function updateArchiveMailbox(accountID: number, mailboxID: number) {
      swipeDraftDirty.current = true;
      setSwipeDraft((current) => ({
        ...current,
        archive_mailboxes: [
          ...current.archive_mailboxes.filter((item) => item.account_id !== accountID),
          ...(mailboxID > 0 ? [{ account_id: accountID, mailbox_id: mailboxID }] : [])
        ]
      }));
    }

    function directionRow(direction: "left" | "right", label: string) {
      const actionKey = `${direction}_action` as const;
      const snoozeKey = `${direction}_snooze_preset` as const;
      return (
        <div className="swipe-direction-row">
          <div className="swipe-direction-label">
            <Icon name={direction === "left" ? "arrow_back" : "arrow_forward"} />
            <strong>{label}</strong>
          </div>
          <div className="swipe-direction-controls">
            <label>
              <span>Action</span>
              <select
                value={swipeDraft[actionKey]}
                onChange={(event) => {
                  swipeDraftDirty.current = true;
                  setSwipeDraft((current) => ({ ...current, [actionKey]: event.target.value as SwipeAction }));
                }}
              >
                {swipeActionChoices.map((choice) => <option value={choice.value} key={choice.value}>{choice.label}</option>)}
              </select>
            </label>
            {swipeDraft[actionKey] === "snooze" ? (
              <label>
                <span>Snooze until</span>
                <select
                  value={swipeDraft[snoozeKey]}
                  onChange={(event) => {
                    swipeDraftDirty.current = true;
                    setSwipeDraft((current) => ({ ...current, [snoozeKey]: event.target.value as SwipeSnoozePreset }));
                  }}
                >
                  {swipeSnoozeChoices.map((choice) => <option value={choice.value} key={choice.value}>{choice.label}</option>)}
                </select>
              </label>
            ) : null}
          </div>
        </div>
      );
    }

    return (
      <form className="panel swipe-settings" onSubmit={saveSwipeSettings}>
        <div className="swipe-direction-list">
          {directionRow("right", "Swipe right")}
          {directionRow("left", "Swipe left")}
        </div>
        {trashUnavailable || missingTrashAccounts.length > 0 ? (
          <small className="swipe-validation">{trashUnavailable ? "Add an IMAP server before using Move to trash." : "Assign a Trash role folder for every IMAP account before using Move to trash."}</small>
        ) : null}
        {archiveRequired ? (
          <section className="swipe-archive-settings">
            <h3>Archive folders</h3>
            <div className="swipe-archive-grid">
              {imapAccounts.map((account) => {
                const choices = archiveChoices(account.id);
                return (
                  <label key={account.id}>
                    <span>{imapAccountLabel(account)}</span>
                    <select
                      value={archiveByAccount.get(account.id) || 0}
                      disabled={choices.length === 0}
                      onChange={(event) => updateArchiveMailbox(account.id, Number(event.target.value))}
                    >
                      <option value={0}>{choices.length === 0 ? "No eligible folders" : "Choose a folder"}</option>
                      {choices.map((mailbox) => <option value={mailbox.id} key={mailbox.id}>{mailbox.name}</option>)}
                    </select>
                  </label>
                );
              })}
            </div>
            {archiveUnavailable || missingArchiveAccounts.length > 0 ? (
              <small className="swipe-validation">{archiveUnavailable ? "Add an IMAP server before using Archive." : "Choose an archive folder for every IMAP account."}</small>
            ) : null}
          </section>
        ) : null}
        <div className="actions">
          <button disabled={loading || savingSwipePreferences || archiveUnavailable || trashUnavailable || missingArchiveAccounts.length > 0 || missingTrashAccounts.length > 0}>
            {savingSwipePreferences ? "Saving..." : "Save swipe actions"}
          </button>
        </div>
      </form>
    );
  }

  function renderSearchSettings() {
    return (
      <form className="panel search-tuning-settings" onSubmit={saveProfile}>
        <p className="muted settings-section-note">These are query-time ranking controls, so changes do not require a reindex.</p>
        <div className="search-tuning-list">
          <SearchSliderRow
            title="Ranking profile"
            value={profileForm.search_preset}
            choices={searchPresetChoices}
            description="Strict reduces expansion, Balanced keeps the current defaults with recent-first ranking, and Forgiving widens typo and attachment matching."
            onChange={(value) => setProfileForm((current) => ({ ...current, ...searchPresetDefaults(value) }))}
          />
          <SearchSliderRow
            title="Typo matching"
            value={profileForm.search_fuzzy}
            choices={fuzzyChoices}
            description="Controls unquoted fuzzy matching for typos. Quoted searches remain literal."
            onChange={(value) => setProfileForm((current) => ({ ...current, search_fuzzy: value }))}
          />
          <SearchSliderRow
            title="Recent mail boost"
            value={profileForm.search_recency_bias}
            choices={recencyChoices}
            description="Normal now favors recent comparable matches; Strong is an aggressive recent-first ranking mode."
            onChange={(value) => setProfileForm((current) => ({ ...current, search_recency_bias: value }))}
          />
          <SearchSliderRow
            title="Attachment text weight"
            value={profileForm.search_attachment_weight}
            choices={attachmentWeightChoices}
            description="Adjusts whether attachment filenames and extracted text are ignored, lightly weighted, normal, or prominent."
            onChange={(value) => setProfileForm((current) => ({ ...current, search_attachment_weight: value }))}
          />
          <SearchSliderRow
            title="Sender history"
            value={profileForm.search_sender_history}
            choices={boostWeightChoices}
            description="Uses your read history to nudge senders you usually open higher in best-match results."
            onChange={(value) => setProfileForm((current) => ({ ...current, search_sender_history: value, search_sender_boost: value !== "none" }))}
          />
          <SearchSliderRow
            title="In contacts"
            value={profileForm.search_contact_boost}
            choices={boostWeightChoices}
            description="Boosts mail from senders saved in your contacts without changing which messages can match."
            onChange={(value) => setProfileForm((current) => ({ ...current, search_contact_boost: value }))}
          />
          <label className="search-tuning-row search-tuning-toggle">
            <div className="search-tuning-copy">
              <strong>Joined words</strong>
              <small>Lets searches like darkroom also consider strong dark room matches.</small>
            </div>
            <input type="checkbox" checked={profileForm.search_compact_splitting} onChange={(event) => setProfileForm((current) => ({ ...current, search_compact_splitting: event.target.checked }))} />
          </label>
        </div>
        <div className="actions"><button>Save search tuning</button></div>
      </form>
    );
  }

  function renderStorageSettings() {
    const indexBreakdown = storageIndexBreakdown(storage);
    const showIndexBreakdown = hasStorageIndexBreakdown(indexBreakdown);
    if (storageLoading) return <SettingsLoading label="Calculating storage usage..." />;
    if (storageError) return <SettingsError message={storageError} onRetry={() => void loadStorage()} />;
    return (
      <section className="panel">
        <div className="storage-grid">
		  <Stat label="Headers" value={formatBytes(storage.DatabaseBytes)} detail={storageEmailDetail(storage.MessageHeaderCount)} />
		  <Stat label="Full Text Search" value={formatBytes(storage.IndexBytes)} detail={storageEmailDetail(storage.IndexMessageCount)} />
		  <Stat label="Local Cache" value={formatBytes(storage.BlobBytes)} detail={storageEmailDetail(storage.MessageBodyCount)} />
          <Stat label="Total" value={formatBytes(storage.TotalBytes)} detail={String(storage.Error || "")} />
        </div>
        {showIndexBreakdown ? (
          <>
			<h3>Full Text Search detail</h3>
            <div className="storage-grid">
              <Stat label="Index segments" value={formatBytes(indexBreakdown.ZapBytes)} detail={`${formatStatCount(indexBreakdown.ZapCount)} files`} />
              <Stat label="Largest segment" value={formatBytes(indexBreakdown.LargestZapBytes)} detail={statDetail(indexBreakdown.LargestZapPath)} />
              <Stat label="Root metadata" value={formatBytes(indexBreakdown.RootBytes)} detail="root.bolt" />
              <Stat label="Other index files" value={formatBytes(indexBreakdown.OtherBytes)} detail={`${formatStatCount(indexBreakdown.FileCount)} total files`} />
            </div>
          </>
        ) : null}
      </section>
    );
  }

  function renderLicenseSettings() {
    return (
      <section className="panel license-panel">
        <p>
          rolltop is free software licensed under the GNU Affero General Public License version 3 or later.
          You may run, study, share, and modify it under that license.
        </p>
        <p>
          The AGPL also applies when modified versions are provided over a network: users of that service must be
          offered access to the Corresponding Source for the version they are using.
        </p>
        <a href="https://www.gnu.org/licenses/agpl-3.0.html" target="_blank" rel="noreferrer">GNU AGPL v3 license text</a>
      </section>
    );
  }

  function renderFolderEditDialog() {
    const folder = editingFolderID ? folderMap.get(editingFolderID) : null;
    if (!folder || !folderDraft) return null;
    const currentIcon = folderDraft.icon || "folder";
    const modeChoices = folderSyncModeChoices(folder.mailbox);
    const selectedSyncMode = modeChoices.some((choice) => choice.value === folderDraft.sync_mode) ? folderDraft.sync_mode : "auto";
    const selectedModeChoice = modeChoices.find((choice) => choice.value === selectedSyncMode);
    const roleConflict = folderRoleConflict(folder, folderDraft.role || "");

    return (
      <div className="folder-dialog-backdrop" role="presentation" onMouseDown={(event) => {
        if (event.target === event.currentTarget) closeFolderSettings();
      }}>
        <form className="folder-edit-dialog" role="dialog" aria-modal="true" aria-labelledby="folder-edit-title" onSubmit={saveEditingFolder}>
          <header className="folder-edit-header">
            <div className="folder-edit-title">
              <span className="folder-edit-icon"><Icon name={currentIcon} /></span>
              <div>
                <h2 id="folder-edit-title">{folder.mailbox.name}</h2>
                <small>{folder.mailbox.account_email || "Mail account"}</small>
              </div>
            </div>
            <button className="folder-icon-action" type="button" onClick={closeFolderSettings} title="Close" aria-label="Close folder settings">
              <Icon name="close" />
            </button>
          </header>

          <div className="folder-edit-body">
            <section className="folder-edit-section">
              <label className="settings-field-label" htmlFor="folder-sync-mode">Sync mode</label>
              <select
                id="folder-sync-mode"
                value={selectedSyncMode}
                onChange={(event) => updateFolderDraft({ sync_mode: event.target.value })}
              >
                {modeChoices.map((choice) => (
                  <option value={choice.value} key={choice.value}>{choice.label}</option>
                ))}
              </select>
              <div className="folder-mode-help">
                {selectedModeChoice ? (
                  <p><strong>{selectedModeChoice.label}:</strong> {selectedModeChoice.description}</p>
                ) : null}
              </div>
            </section>

            <section className="folder-edit-section">
              <label className="settings-field-label" htmlFor="folder-role">Folder role</label>
              <select
                id="folder-role"
                value={folderDraft.role || ""}
                onChange={(event) => updateFolderDraft({ role: event.target.value })}
              >
                {folderRoleChoices.map((choice) => {
                  const assigned = choice.value ? folderRoleConflict(folder, choice.value) : null;
                  return (
                    <option value={choice.value} key={choice.value || "normal"} disabled={Boolean(assigned)}>
                      {choice.label}{assigned ? ` - used by ${assigned.mailbox.name}` : ""}
                    </option>
                  );
                })}
              </select>
              {roleConflict ? <div className="folder-role-warning">{folderRoleLabel(folderDraft.role || "")} is already assigned to {roleConflict.mailbox.name}.</div> : null}
            </section>

            <section className="folder-edit-section folder-edit-section-wide">
              <span className="settings-field-label">Sidebar icon</span>
              <div className="folder-icon-grid" aria-label="Sidebar icon">
                {folderIconChoices.map((choice) => (
                  <button
                    className={currentIcon === choice.value ? "active" : ""}
                    type="button"
                    key={choice.value}
                    onClick={() => updateFolderDraft({ icon: choice.value })}
                    title={choice.label}
                    aria-label={choice.label}
                  >
                    <Icon name={choice.value} weight={currentIcon === choice.value ? "bold" : undefined} />
                  </button>
                ))}
              </div>
            </section>

            <section className="folder-edit-section">
              <span className="settings-field-label">Visible in</span>
              <div className="folder-edit-visibility">
                {folderVisibilityChoices.map((choice) => {
                  const active = Boolean(folderDraft[choice.key]);
                  return (
                    <button
                      className={active ? "active" : ""}
                      type="button"
                      key={choice.key}
                      onClick={() => updateFolderDraft({ [choice.key]: !active } as Partial<FolderSettingsDraft>)}
                      aria-pressed={active}
                    >
                      {choice.label}
                    </button>
                  );
                })}
              </div>
            </section>

            <section className="folder-edit-section folder-edit-section-wide folder-index-section">
              <span className="settings-field-label">Local index</span>
              <div className="folder-purge-note">These actions only change Rolltop's local cache. They never delete messages from your IMAP server.</div>
              <div className="folder-purge-actions">
                <button
                  className="secondary folder-purge-button"
                  type="button"
                  disabled={folder.is_running || Boolean(folderMaintenance) || !folder.mailbox.include_in_search}
                  aria-busy={folderMaintenance?.mailboxID === folder.mailbox.id && folderMaintenance.action === "rebuild"}
                  onClick={() => rebuildFolderSearchIndex(folder)}
                  title={!folder.mailbox.include_in_search
                    ? "Enable Search visibility before rebuilding this folder"
                    : folder.is_running
                      ? "Wait for this folder's current work to finish"
                      : "Replace this folder's local full-text search documents"}
                >
                  <Icon name="sync" />
                  {folderMaintenance?.mailboxID === folder.mailbox.id && folderMaintenance.action === "rebuild"
                    ? "Starting rebuild..."
                    : "Rebuild full-text index"}
                </button>
                <button
                  className="secondary folder-purge-button"
                  type="button"
                  disabled={folder.is_running || Boolean(folderMaintenance)}
                  aria-busy={folderMaintenance?.mailboxID === folder.mailbox.id && folderMaintenance.action === "purge-index"}
                  onClick={() => purgeFolderSearchIndex(folder)}
                  title={folder.is_running ? "Wait for this folder's current sync to finish" : "Purge only the local full-text search index"}
                >
                  <Icon name="search" />
                  {folderMaintenance?.mailboxID === folder.mailbox.id && folderMaintenance.action === "purge-index"
                    ? "Starting purge..."
                    : "Purge full-text index"}
                </button>
                <button
                  className="secondary folder-purge-button danger"
                  type="button"
                  disabled={folder.is_running || Boolean(folderMaintenance)}
                  aria-busy={folderMaintenance?.mailboxID === folder.mailbox.id && folderMaintenance.action === "purge-references"}
                  onClick={() => purgeFolderLocalReferences(folder)}
                  title={folder.is_running ? "Wait for this folder's current sync to finish" : "Purge local references and the local full-text search index"}
                >
                  <Icon name="delete" />
                  {folderMaintenance?.mailboxID === folder.mailbox.id && folderMaintenance.action === "purge-references"
                    ? "Starting purge..."
                    : "Purge references and index"}
                </button>
              </div>
            </section>
          </div>

          <footer className="folder-edit-footer">
            <button className="secondary" type="button" onClick={closeFolderSettings}>Cancel</button>
            <button type="submit" disabled={Boolean(roleConflict)}>Save settings</button>
          </footer>
        </form>
      </div>
    );
  }

  function renderFolderSettings() {
    return (
      <>
        <section className="panel folder-settings-panel">
          <div className="folder-settings-header">
            <div>
              <h2>Folder sync</h2>
              <div className="muted">Folders under {selectedAccountLabel}</div>
            </div>
            {selectedAccountID ? (
              <form className="folder-create-form" onSubmit={createFolder}>
                <input
                  value={newFolderName}
                  onChange={(event) => setNewFolderName(event.target.value)}
                  placeholder="New folder"
                  aria-label="New IMAP folder name"
                  disabled={creatingFolder}
                />
                <button className="secondary" type="submit" disabled={creatingFolder || !newFolderName.trim()}>
                  <Icon name="create_new_folder" />Create
                </button>
              </form>
            ) : null}
          </div>
          <div className="folder-sync-list">
            {folderNodes.length > 0 ? renderFolderItems(folderNodes) : <div className="muted">No folders discovered yet. Sync this account to discover folders.</div>}
          </div>
        </section>
        {renderFolderEditDialog()}
      </>
    );
  }

  function renderRecentRuns() {
    return (
      <section className="panel">
        <h2>Recent sync runs</h2>
        <div className="settings-table-scroll" role="region" aria-label="Recent sync runs" tabIndex={0}>
          <table>
            <thead><tr><th>Status</th><th>Folder</th><th>Messages</th><th>Updated</th></tr></thead>
            <tbody>
              {runs.map((run) => (
                <tr key={run.id}>
                  <td><button className="link-button" type="button" onClick={() => navigate(`/sync-runs/${run.id}`)}>{run.status}</button></td>
                  <td>{run.current_mailbox}</td>
                  <td>{run.messages_stored} processed, {run.messages_skipped} skipped</td>
                  <td>{displayDateTime(run.updated_at, user)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    );
  }

  const displayName = user.name || user.email;
  const themeName = (availableThemes.length > 0 ? availableThemes : fallbackThemes())
    .find((theme) => theme.id === profileForm.theme)?.name || "Classic";
  const swipeLabel = (action: SwipeAction) => swipeActionChoices.find((choice) => choice.value === action)?.label || action;
  const sectionForRoute = (): SettingsSectionID => {
    if (matchedPluginRoute) return matchedPluginRoute.section || "plugins";
    if (route.kind === "plugins" || route.kind === "unknown") return "plugins";
    if (["mail", "imap", "smtp", "identities"].includes(route.kind)) return "mail";
    if (["preferences", "swipes", "search"].includes(route.kind)) return "preferences";
    return "general";
  };

  const noticeNode = notice ? <div className="notice">{notice}</div> : null;
  let page: ReactNode;

  if (matchedPluginRoute) {
    page = matchedPluginRoute.render({ csrf, user, mailboxes, location, navigate, addToast });
  } else if (route.kind === "unknown" && runtimePlugins.status === "loading") {
    page = <SettingsLoading label="Loading plugin settings..." />;
  } else if (route.kind === "unknown" && runtimePlugins.errors.length > 0) {
    page = (
      <SettingsPage title="Plugin settings unavailable" description="An enabled plugin did not finish loading." backPath="/settings/account/plugins" navigate={navigate}>
        <SettingsError
          message={`${runtimePlugins.errors.length} enabled plugin${runtimePlugins.errors.length === 1 ? "" : "s"} could not load.`}
          onRetry={() => void reloadRuntimePlugins()}
        />
      </SettingsPage>
    );
  } else if (route.kind === "unknown") {
    page = (
      <SettingsPage title="Settings page unavailable" description="This settings page is not registered or its plugin could not load." backPath="/settings/account/plugins" navigate={navigate}>
        <SettingsEmpty icon="settings" title="No settings page found" description="Return to Plugins to choose an available settings page." />
      </SettingsPage>
    );
  } else if (route.kind === "plugins") {
    page = (
      <SettingsPage title="Plugins" description="Settings supplied by enabled Rolltop plugins." navigate={navigate}>
        {runtimePlugins.status === "loading" ? <SettingsLoading label="Loading plugin settings..." /> : null}
        {runtimePlugins.errors.length > 0 ? (
          <SettingsError
            message={`${runtimePlugins.errors.length} enabled plugin${runtimePlugins.errors.length === 1 ? "" : "s"} could not load.`}
            onRetry={() => void reloadRuntimePlugins()}
          />
        ) : null}
        {runtimePlugins.status === "ready" && pluginRoutes.length === 0 ? (
          <SettingsEmpty icon="settings" title="No plugin settings" description="Enabled plugins with configurable settings will appear here." />
        ) : null}
        {pluginRoutes.length > 0 ? (
          <SettingsIndex ariaLabel="Plugin settings">
            {pluginRoutes.map((pluginRoute) => (
              <SettingsIndexRow
                key={pluginRoute.path}
                icon={pluginRoute.icon}
                title={pluginRoute.label}
                description={pluginRoute.description}
                path={pluginRoute.path}
                navigate={navigate}
              />
            ))}
          </SettingsIndex>
        ) : null}
      </SettingsPage>
    );
  } else if (!loaded) {
    page = loadError
      ? <SettingsError message={loadError} onRetry={() => void load()} />
      : <SettingsLoading />;
  } else if (route.kind === "identities") {
    page = (
      <SettingsPage
        title={<>Identities <span className="label-pill">{identities.length.toLocaleString()}</span></>}
        description="Outgoing names, delivery servers, Sent and Drafts folders, signatures, and identity security."
        backPath="/settings/account/mail"
        navigate={navigate}
        actions={<button className="secondary" type="button" onClick={newIdentity}><Icon name="edit" />New identity</button>}
      >
        {noticeNode}
        {renderIdentitySettings()}
      </SettingsPage>
    );
  } else if (route.kind === "imap") {
    page = !route.isNew && !account ? (
      <SettingsPage title="IMAP server unavailable" backPath="/settings/account/mail" navigate={navigate}>
        <SettingsEmpty icon="inbox" title="Server not found" description="It may have been removed or may not belong to this account." />
      </SettingsPage>
    ) : (
      <SettingsPage
        title={selectedAccountLabel}
        description={route.isNew ? "Connect another incoming mail server." : "Connection, sync scope, folders, and recent activity."}
        backPath="/settings/account/mail"
        navigate={navigate}
        actions={route.isNew ? null : <>
          <button type="button" onClick={syncNow}><Icon name="sync" />Sync all</button>
          <button
            className="secondary"
            type="button"
            disabled={Boolean(accountSearchRebuildID) || Boolean(folderMaintenance) || selectedFolders.some((folder) => folder.is_running)}
            aria-busy={accountSearchRebuildID === account?.id}
            onClick={rebuildAccountSearchIndexes}
            title="Replace the local full-text index for every searchable folder on this IMAP server"
          >
            <Icon name="search" />{accountSearchRebuildID === account?.id ? "Starting rebuild..." : "Rebuild full-text indexes"}
          </button>
        </>}
      >
        {noticeNode}
        <form className="panel account-settings" onSubmit={save}>
          <h2>IMAP server</h2>
          <div className="settings-columns account-settings-grid">
            <section>
              <h3>Connection</h3>
              <Field label="Label" value={form.label} onChange={(value) => setField("label", value)} placeholder="Personal mail, Work archive" />
              <Field label="Email" value={form.email} onChange={(value) => setField("email", value)} type="email" />
              <Field label="Host" value={form.host} onChange={(value) => setField("host", value)} />
              <Field label="Port" value={form.port} onChange={(value) => setField("port", value)} type="number" />
              <Field label="Username" value={form.username} onChange={(value) => setField("username", value)} />
              <Field
                label="Password"
                value={form.password}
                onChange={(value) => setField("password", value)}
                type="password"
                placeholder={accountNeedsPassword ? "Required to restore IMAP access" : account ? "Leave blank to keep current password" : ""}
                required={accountNeedsPassword || !account}
              />
              <label><input type="checkbox" checked={form.use_tls} onChange={(event) => setField("use_tls", event.target.checked)} /> Use TLS</label>
            </section>
            <section>
              <h3>Sync scope</h3>
              <Field label="Folders" value={form.mailbox} onChange={(value) => setField("mailbox", value)} placeholder="*" />
              <Field label="Interval minutes" value={form.sync_interval_minutes} onChange={(value) => setField("sync_interval_minutes", value)} type="number" />
            </section>
          </div>
          <div className="actions split-actions">
            <button>Save IMAP server</button>
            {account ? (
              <button className="danger secondary" type="button" disabled={deletingAccountID === account.id} onClick={deleteSelectedIMAPAccount}>
                <Icon name="delete" />{deletingAccountID === account.id ? "Removing..." : "Remove IMAP Server"}
              </button>
            ) : null}
          </div>
        </form>
        {route.isNew ? null : renderFolderSettings()}
        {route.isNew ? null : renderRecentRuns()}
      </SettingsPage>
    );
  } else if (route.kind === "smtp") {
    page = !route.isNew && !selectedSMTP ? (
      <SettingsPage title="SMTP server unavailable" backPath="/settings/account/mail" navigate={navigate}>
        <SettingsEmpty icon="send" title="Server not found" description="It may have been removed or may not belong to this account." />
      </SettingsPage>
    ) : (
      <SettingsPage title={selectedSMTPLabel} description="Connection details used by outgoing identities." backPath="/settings/account/mail" navigate={navigate}>
        {noticeNode}
        <form className="panel smtp-settings-form" onSubmit={saveSMTP}>
          <h2>SMTP server</h2>
          <div className="settings-columns display-settings-grid">
            <section>
              <Field label="Label" value={smtpForm.label} onChange={(value) => setSMTPField("label", value)} />
              <Field label="Host" value={smtpForm.host} onChange={(value) => setSMTPField("host", value)} />
              <Field label="Port" value={smtpForm.port} onChange={(value) => setSMTPField("port", value)} type="number" />
            </section>
            <section>
              <Field label="Username" value={smtpForm.username} onChange={(value) => setSMTPField("username", value)} />
              <Field label="Password" value={smtpForm.password} onChange={(value) => setSMTPField("password", value)} type="password" placeholder={selectedSMTPID ? "Leave blank to keep current password" : ""} />
              <label><input type="checkbox" checked={smtpForm.use_tls} onChange={(event) => setSMTPField("use_tls", event.target.checked)} /> Use TLS / STARTTLS</label>
            </section>
          </div>
          <div className="actions split-actions">
            <button>Save SMTP server</button>
            {selectedSMTP ? (
              <button className="danger secondary" type="button" disabled={deletingSMTPID === selectedSMTP.id} onClick={deleteSelectedSMTPAccount}>
                <Icon name="delete" />{deletingSMTPID === selectedSMTP.id ? "Removing..." : "Remove SMTP Server"}
              </button>
            ) : null}
          </div>
        </form>
      </SettingsPage>
    );
  } else if (route.kind === "profile") {
    page = <SettingsPage title="Profile" description="Account identity and recovery settings." backPath="/settings/account/general" navigate={navigate}>{noticeNode}{renderProfileSettings()}</SettingsPage>;
  } else if (route.kind === "display") {
    page = <SettingsPage title="Display" description="Theme, locale, and date formatting." backPath="/settings/account/general" navigate={navigate}>{noticeNode}{renderDisplaySettings()}</SettingsPage>;
  } else if (route.kind === "storage") {
	page = <SettingsPage title="Storage" description="Headers, Local Cache, and Full Text Search usage." backPath="/settings/account/general" navigate={navigate}>{renderStorageSettings()}</SettingsPage>;
  } else if (route.kind === "about") {
    page = <SettingsPage title="About Rolltop" description="Software license and source terms." backPath="/settings/account/general" navigate={navigate}>{renderLicenseSettings()}</SettingsPage>;
  } else if (route.kind === "swipes") {
    page = <SettingsPage title="Swipe actions" description="Choose what left and right swipes do on touch devices." backPath="/settings/account/preferences" navigate={navigate}>{noticeNode}{renderSwipeSettings()}</SettingsPage>;
  } else if (route.kind === "search") {
    page = <SettingsPage title="Search tuning" description="Control typo tolerance, ranking, and attachment matching." backPath="/settings/account/preferences" navigate={navigate}>{noticeNode}{renderSearchSettings()}</SettingsPage>;
  } else if (route.kind === "mail") {
    page = (
      <SettingsPage title="Mail" description="Incoming servers, outgoing delivery, folders, and identities." navigate={navigate}>
        {noticeNode}
        <section className="settings-index-group">
          <div className="settings-index-heading">
            <div><h2>Incoming mail</h2><p>IMAP connections, folder sync, roles, and local indexing.</p></div>
            <button className="secondary" type="button" onClick={newIMAPAccount}><Icon name="add" />Add IMAP</button>
          </div>
          {imapAccounts.length > 0 ? (
            <SettingsIndex ariaLabel="IMAP servers">
              {imapAccounts.map((item) => (
                <SettingsIndexRow key={item.id} icon="inbox" title={item.label || item.email} description={`${item.email} · ${item.host}:${item.port}`} meta="IMAP" path={`/settings/account/mail/imap/${item.id}`} navigate={navigate} onNavigate={() => selectIMAP(item)} />
              ))}
            </SettingsIndex>
          ) : <SettingsEmpty icon="inbox" title="No IMAP servers" description="Add an incoming mail server to begin mirroring mail." />}
        </section>
        <section className="settings-index-group">
          <div className="settings-index-heading">
            <div><h2>Outgoing mail</h2><p>SMTP delivery servers assigned to outgoing identities.</p></div>
            <button className="secondary" type="button" onClick={newSMTPAccount}><Icon name="add" />Add SMTP</button>
          </div>
          {smtpAccounts.length > 0 ? (
            <SettingsIndex ariaLabel="SMTP servers">
              {smtpAccounts.map((item) => (
                <SettingsIndexRow key={item.id} icon="send" title={item.label || item.host} description={`${item.username || "No username"} · ${item.host}:${item.port}`} meta={`${(identitiesBySMTP.get(item.id) || []).length} identities`} path={`/settings/account/mail/smtp/${item.id}`} navigate={navigate} onNavigate={() => selectSMTP(item)} />
              ))}
            </SettingsIndex>
          ) : <SettingsEmpty icon="send" title="No SMTP servers" description="Add an outgoing server or use the delivery server configured during setup." />}
        </section>
        <section className="settings-index-group">
          <div className="settings-index-heading"><div><h2>Sender identities</h2><p>Names, addresses, signatures, folders, and message security.</p></div></div>
          <SettingsIndex ariaLabel="Identity settings">
            <SettingsIndexRow icon="group" title="Identities" description={identities.length > 0 ? `${identities.length} configured outgoing ${identities.length === 1 ? "identity" : "identities"}.` : "Create the first outgoing identity."} meta={identities.length === 1 ? "1 identity" : `${identities.length} identities`} path="/settings/account/mail/identities" navigate={navigate} />
          </SettingsIndex>
        </section>
      </SettingsPage>
    );
  } else if (route.kind === "preferences") {
    page = (
      <SettingsPage title="Preferences" description="Message gestures and search behavior." navigate={navigate}>
        <SettingsIndex ariaLabel="Mail preferences">
          <SettingsIndexRow icon="arrow_back" title="Swipe actions" description="Configure left and right gestures, archive folders, and snooze timing." meta={`Left: ${swipeLabel(swipeDraft.left_action)} · Right: ${swipeLabel(swipeDraft.right_action)}`} path="/settings/account/preferences/swipes" navigate={navigate} />
          <SettingsIndexRow icon="search" title="Search tuning" description="Adjust ranking, typo matching, contacts, and attachment text." meta={profileForm.search_preset || "Balanced"} path="/settings/account/preferences/search" navigate={navigate} />
        </SettingsIndex>
      </SettingsPage>
    );
  } else {
    page = (
      <SettingsPage title="General" description="Your profile, interface, local storage, and Rolltop information." navigate={navigate}>
        {noticeNode}
        <SettingsIndex ariaLabel="General settings">
          <SettingsIndexRow icon="group" title="Profile" description="Signed-in identity and password-recovery address." meta={displayName} path="/settings/account/general/profile" navigate={navigate} />
          <SettingsIndexRow icon="settings" title="Display" description="Theme, locale, and date formatting." meta={themeName} path="/settings/account/general/display" navigate={navigate} />
		  <SettingsIndexRow icon="mail" title="Storage" description="Headers, Local Cache, and Full Text Search usage." meta={storageError ? "Unavailable" : storageLoading ? "Calculating" : formatBytes(storage.TotalBytes)} path="/settings/account/general/storage" navigate={navigate} />
          <SettingsIndexRow icon="file_text" title="About" description="Rolltop license and source terms." meta="AGPL v3+" path="/settings/account/general/about" navigate={navigate} />
        </SettingsIndex>
      </SettingsPage>
    );
  }

  return (
    <SettingsShell activeSection={sectionForRoute()} navigate={navigate}>
      {loading && loaded && !matchedPluginRoute ? <div className="settings-refreshing" role="status" aria-label="Refreshing settings"><span /></div> : null}
      {loadError && loaded && !matchedPluginRoute ? <SettingsError message={loadError} onRetry={() => void load()} /> : null}
      {page}
    </SettingsShell>
  );
}


/** AdminUsersView lets an admin create local users and refreshes chrome after user changes. */
export function AdminUsersView({
  csrf,
  refreshChrome,
  addToast
}: {
  csrf: string;
  refreshChrome: () => Promise<Bootstrap | null>;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [users, setUsers] = useState<User[]>([]);
  const [plugins, setPlugins] = useState<PluginSetting[]>([]);
  const [form, setForm] = useState({ email: "", name: "", password: "", is_admin: false });
  const [passwordResetFromAddress, setPasswordResetFromAddress] = useState("");
  const [passwords, setPasswords] = useState<Record<number, string>>({});

  const load = useCallback(async () => {
    const userData = await api.users();
    setUsers(userData.users);
    setPasswordResetFromAddress(userData.password_reset_from_address || "");
  }, []);

  useEffect(() => {
    void load().catch((err) => addToast(messageFromError(err), "error"));
  }, [addToast, load]);

  async function create(event: FormEvent) {
    event.preventDefault();
    try {
      await api.createUser(csrf, form);
      setForm({ email: "", name: "", password: "", is_admin: false });
      addToast("User created.");
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function savePasswordResetSettings(event: FormEvent) {
    event.preventDefault();
    try {
      const saved = await api.savePasswordResetSettings(csrf, passwordResetFromAddress);
      setPasswordResetFromAddress(saved.from_address || "");
      addToast("Password reset settings saved.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function changePassword(user: User) {
    const password = passwords[user.id] || "";
    if (password.length < 12) {
      addToast("Password must be at least 12 characters.", "error");
      return;
    }
    try {
      await api.setUserPassword(csrf, user.id, password);
      setPasswords((current) => ({ ...current, [user.id]: "" }));
      addToast(`Password changed for ${user.email}.`);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function deleteAccount(user: User) {
    if (!window.confirm(`Delete ${user.email} from rolltop? This removes the local account and local rolltop data for that user. It does not delete remote IMAP mail.`)) return;
    try {
      await api.deleteUser(csrf, user.id);
      addToast(`Deleted ${user.email}.`);
      await load();
      await refreshChrome();
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  const remoteImagePlugin = plugins.find((plugin) => plugin.id === pluginIDs.remoteImageBlocklist);

  return (
    <>
      <div className="content-head"><h1>Admin</h1></div>
      <form className="panel" onSubmit={create}>
        <h2>Create user</h2>
        <div className="grid">
          <Field label="Email" value={form.email} onChange={(value) => setForm((current) => ({ ...current, email: value }))} type="email" />
          <Field label="Name" value={form.name} onChange={(value) => setForm((current) => ({ ...current, name: value }))} />
          <Field label="Password" value={form.password} onChange={(value) => setForm((current) => ({ ...current, password: value }))} type="password" />
        </div>
        <div className="checks">
          <label>
            <input
              type="checkbox"
              checked={form.is_admin}
              onChange={(event) => setForm((current) => ({ ...current, is_admin: event.target.checked }))}
            /> Admin
          </label>
        </div>
        <button>Create user</button>
      </form>
      <section className="panel">
        <h2>Password resets</h2>
        <form className="admin-reset-settings" onSubmit={savePasswordResetSettings}>
          <div>
            <label>Password Reset from address</label>
            <input
              type="email"
              value={passwordResetFromAddress}
              placeholder="Defaults to the signed-in admin address"
              onChange={(event) => setPasswordResetFromAddress(event.target.value)}
            />
            <small>Reset links are sent as ordinary unencrypted email to the user's backup email address.</small>
          </div>
          <button className="secondary" type="submit">Save reset settings</button>
        </form>
      </section>
      <section className="panel">
        <h2>Existing users</h2>
        <table>
          <thead>
            <tr><th>Email</th><th>Name</th><th>Backup email</th><th>Role</th><th>Admin actions</th></tr>
          </thead>
          <tbody>
            {users.map((user) => (
              <tr key={user.id}>
                <td>{user.email}</td>
                <td>{user.name}</td>
                <td>{user.backup_email || "Not set"}</td>
                <td>{user.is_admin ? "Admin" : "User"}</td>
                <td>
                  <div className="admin-user-actions">
                    <input
                      type="password"
                      placeholder="New password"
                      value={passwords[user.id] || ""}
                      onChange={(event) => setPasswords((current) => ({ ...current, [user.id]: event.target.value }))}
                    />
                    <button className="secondary" type="button" onClick={() => void changePassword(user)}>Change</button>
                    <button className="danger" type="button" onClick={() => void deleteAccount(user)}>Delete</button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
      <PluginTogglePanel
        csrf={csrf}
        addToast={addToast}
        onPluginsChanged={setPlugins}
        onPluginSaved={refreshChrome}
      />
      <AdminRemoteImageBlocklist csrf={csrf} addToast={addToast} enabled={Boolean(remoteImagePlugin?.enabled)} />
    </>
  );
}

type SyncStage = "inspect" | "prepare" | "transfer" | "index" | "finish";

const syncRunStages: Array<{ id: SyncStage; title: string; detail: string; icon: string }> = [
  { id: "inspect", title: "Inspect", detail: "Connect and read folder state", icon: "travel_explore" },
  { id: "prepare", title: "Prepare", detail: "Validate the local checkpoint", icon: "folder_open" },
  { id: "transfer", title: "Transfer", detail: "Fetch and save new messages", icon: "download" },
  { id: "index", title: "Index", detail: "Make saved mail searchable", icon: "manage_search" },
  { id: "finish", title: "Finish", detail: "Checkpoint and update flags", icon: "check_circle" }
];

function syncStageForPhase(phase: string, run: SyncRun): SyncStage {
  const value = phase.trim().toLowerCase();
  if (run.latest_new_from === "rolltop:maintenance") return "index";
  if (/^(starting|plugin-discovery|sqlite-create-sync-run|mailbox-selection|sqlite-last-uids|imap-mailbox-status|imap-uid-snapshot)/.test(value)) return "inspect";
  if (/(generation-state|sqlite-get-mailbox|plugin-hook-discovery|prepare|reconcile)/.test(value)) return "prepare";
  if (/(search|language-detect|plugin-classification)/.test(value)) return "index";
  if (/(checkpoint|sync-progress|metadata|seen-flags|flagged-flags|complete)/.test(value)) return "finish";
  return "transfer";
}

function syncPhaseCopy(phase: string, detail: string, run: SyncRun): { title: string; detail: string } {
  const value = phase.trim().toLowerCase();
  const folder = detail || run.current_mailbox || "this folder";
  if (!value || value === "starting") return { title: "Starting sync", detail: "Preparing the mailbox job." };
  if (value === "imap-mailbox-status") return { title: "Reading folder status", detail: `Checking remote headers for ${folder}.` };
  if (value === "imap-uid-snapshot") return { title: "Inspecting message IDs", detail: `Building a safe snapshot for ${folder}.` };
  if (value === "sqlite-last-uids" || value === "sqlite-generation-state") return { title: "Checking the local checkpoint", detail: "Comparing the remote mailbox with locally stored mail." };
  if (value === "sqlite-message-exists") return { title: "Saving new mail", detail: "Checking whether this message is already stored." };
  if (value === "mime-parse") return { title: "Reading message content", detail: "Extracting headers and message text." };
  if (value === "blob-save" || value === "sqlite-create-blob") return { title: "Saving local cache", detail: "Writing the original message safely to local storage." };
  if (value.startsWith("search-")) return { title: "Updating full text search", detail: "Adding saved mail to the local search index." };
  if (value === "language-detect") return { title: "Preparing search", detail: "Detecting message language for better search." };
  if (value.includes("classification")) return { title: "Applying message checks", detail: "Running enabled local message classifiers." };
  if (value.includes("checkpoint")) return { title: "Saving progress", detail: "Writing a checkpoint so this sync can resume safely." };
  if (value.includes("reconcile") || value.includes("flags")) return { title: "Refreshing message state", detail: "Updating read and starred status without changing remote mail." };
  if (value.includes("fetch")) return { title: "Fetching mail", detail: `Downloading the next messages from ${folder}.` };
  return { title: "Processing mailbox", detail: detail || "Working through the next safe sync step." };
}

function syncRunProgress(run: SyncRun): { percent: number | null; label: string } {
  const total = Math.max(0, run.messages_total || 0);
  const seen = Math.max(0, run.messages_seen || 0);
  if (total > 0) {
    return { percent: Math.min(100, Math.round((seen / total) * 100)), label: `${Math.min(seen, total).toLocaleString()} of ${total.toLocaleString()} messages checked` };
  }
  if (run.mailboxes_total > 0) {
    return { percent: Math.min(100, Math.round((Math.max(0, run.mailboxes_done) / run.mailboxes_total) * 100)), label: `${Math.max(0, run.mailboxes_done)} of ${run.mailboxes_total} folders checked` };
  }
  if (run.messages_stored > 0 || run.messages_skipped > 0) {
    return { percent: null, label: `${run.messages_stored.toLocaleString()} processed · ${run.messages_skipped.toLocaleString()} skipped` };
  }
  return { percent: null, label: run.status === "running" ? "Determining what needs syncing" : "No new messages needed processing" };
}

function syncStatusLabel(status: string): string {
  const value = status.trim().toLowerCase();
  if (value === "running") return "In progress";
  if (value === "ok") return "Complete";
  return value ? value[0].toUpperCase() + value.slice(1) : "Unknown";
}

/** SyncRunView shows a single sync run's live activity and durable progress. */
export function SyncRunView({
  csrf,
  location,
  navigate,
  datePrefs
}: {
  csrf: string;
  location: LocationState;
  navigate: (url: string) => void;
  datePrefs: DatePrefs;
}) {
  const id = location.path.split("/").pop() || "";
  const [run, setRun] = useState<SyncRun | null>(null);
  const [live, setLive] = useState<SyncRunLiveDetail | null>(null);
  const [error, setError] = useState("");
  const [cancelling, setCancelling] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const refresh = () => api.syncRun(id).then((data) => {
      if (!cancelled) { setRun(data.sync_run); setLive(data.live); }
    }).catch((err) => { if (!cancelled) setError(messageFromError(err)); });
    void refresh();
    const timer = window.setInterval(() => { void refresh(); }, 1500);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [id]);

  const cancel = async () => {
    if (!run || !live?.cancellable || cancelling) return;
    setCancelling(true);
    setError("");
    try {
      await api.cancelSyncRun(csrf, run.id);
      const data = await api.syncRun(id);
      setRun(data.sync_run);
      setLive(data.live);
    } catch (err) {
      setError(messageFromError(err));
    } finally {
      setCancelling(false);
    }
  };

  const phase = run ? syncPhaseCopy(live?.phase || "", live?.detail || "", run) : null;
  const activeStage = run ? syncStageForPhase(live?.phase || "", run) : "inspect";
  const progress = run ? syncRunProgress(run) : null;
  const progressInfo = progress || { percent: null, label: "Loading sync progress" };
  const activeStageIndex = syncRunStages.findIndex((stage) => stage.id === activeStage);
  const isActive = Boolean(run && run.status === "running" && live?.active);

  return (
    <>
      <div className="content-head">
        <h1>Sync run</h1>
        <button className="secondary" type="button" onClick={() => navigate("/settings/account/mail")}>Back to mail settings</button>
      </div>
      {error ? <div className="error">{error}</div> : null}
      {run ? (
        <section className={`sync-run-detail panel ${isActive ? "running" : ""}`}>
          <div className="sync-run-hero">
            <div className={`sync-run-state ${run.status}`}><Icon name={isActive ? "sync" : run.status === "ok" ? "check" : "info"} /></div>
            <div>
              <div className={`sync-run-status ${run.status}`}>{syncStatusLabel(run.status)}</div>
              <h2>{phase?.title}</h2>
              <p>{phase?.detail}</p>
            </div>
            {live?.cancellable ? <button className="danger sync-run-cancel" type="button" onClick={() => void cancel()} disabled={cancelling}>{cancelling ? "Cancelling…" : live.active ? "Cancel sync" : "Clear stale run"}</button> : null}
          </div>

          <div className="sync-run-progress-block">
            <div className="sync-run-progress-copy"><strong>{progressInfo.label}</strong><span>{progressInfo.percent !== null ? `${progressInfo.percent}%` : "Live"}</span></div>
            <div className={`sync-run-progress ${progressInfo.percent === null ? "indeterminate" : ""}`}><div style={progressInfo.percent === null ? undefined : { width: `${progressInfo.percent}%` }} /></div>
          </div>

          <ol className="sync-run-stages" aria-label="Expected sync steps">
            {syncRunStages.map((stage, index) => {
              const current = isActive && stage.id === activeStage;
              const complete = !isActive ? run.status === "ok" : index < activeStageIndex;
              return <li key={stage.id} className={current ? "current" : complete ? "complete" : ""}>
                <span className="sync-run-stage-icon"><Icon name={complete ? "check" : stage.icon} /></span>
                <span><strong>{stage.title}</strong><small>{stage.detail}</small></span>
              </li>;
            })}
          </ol>

          <div className="sync-run-facts">
            <div><span>Folder</span><strong>{run.current_mailbox || "Account-wide check"}</strong></div>
            <div><span>Messages</span><strong>{run.messages_stored.toLocaleString()} processed · {run.messages_skipped.toLocaleString()} skipped</strong></div>
            <div><span>Checkpoint</span><strong>{run.current_uid ? `UID ${run.current_uid.toLocaleString()}` : "Awaiting folder status"}</strong></div>
            <div><span>Last update</span><strong>{displayDateTime(run.updated_at, datePrefs)}</strong></div>
          </div>

          {!live?.active && run.status === "running" ? <div className="sync-run-stale">No active worker is attached to this run. It will be marked interrupted automatically, or you can clear it now.</div> : null}
          {run.error ? <div className="sync-run-error"><Icon name="error" />{run.error}</div> : null}
        </section>
      ) : (
        <div className="panel muted">Loading sync run...</div>
      )}
    </>
  );
}
