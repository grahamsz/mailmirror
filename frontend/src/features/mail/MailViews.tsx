// File overview: Mailbox and search result lists. These components fetch paged conversations,
// surface sync clues, keep selection state stable, and link rows back to their source page.

import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties, DragEvent, KeyboardEvent, MouseEvent, ReactNode, TouchEvent } from "react";
import { Star } from "@phosphor-icons/react";
import { ApiError, api, bulkMessageIDLimit } from "../../api";
import type { AddToast, DatePrefs, LocationState } from "../../appTypes";
import type { Bootstrap, Conversation, Mailbox, SwipeAction, SwipePreferences, SyncRun } from "../../types";
import { Icon } from "../../components/Icon";
import { ListHeader } from "../../components/common";
import { androidNativeAvailable } from "../../lib/androidNative";
import { messageFromError } from "../../lib/errors";
import { displaySnoozeUntil, displayTime, messageCountLabel } from "../../lib/format";
import { trashMailboxForAccount } from "../../lib/folders";
import { shouldIgnoreMailShortcut } from "../../lib/keyboard";
import { effectiveMailboxSyncMode, mailboxActiveRun, mailboxNeedsSync, mailboxRefreshKey } from "../../lib/sync";
import { HighlightedText } from "../../lib/searchHighlight";
import { mailPageSize } from "../../lib/constants";
import { usePullToRefresh } from "../../lib/pullToRefresh";
import { mailRoute, mailURL, messageURL, routeWithSearch, searchRoute, searchURL } from "../../lib/routes";
import { messageSecurityIndicators, messageSecurityPreviewText, messageSecuritySnippetClassName } from "../../plugins/messageSecurity";
import type { RuntimePlugin } from "../../plugins/runtime";
import { defaultSwipePreferences, swipeActionPresentation, swipeSnoozeUntil } from "../../lib/swipeActions";
import { SnoozeControl } from "./SnoozeControl";

type SearchActionPlugin = RuntimePlugin & {
  renderSearchActions?: (context: {
    query: string;
    navigate: (url: string) => void;
  }) => ReactNode;
};

type MessageAnnotationPlugin = RuntimePlugin & {
  renderMessageAnnotations?: (context: {
    location: "message-list" | "thread";
    message: Conversation["message"];
    annotations: NonNullable<Conversation["message"]["annotations"]>;
  }) => ReactNode;
};

function searchActionNodes(plugins: RuntimePlugin[], query: string, navigate: (url: string) => void) {
  if (!query) return [];
  return (plugins as SearchActionPlugin[])
    .map((plugin) => plugin.renderSearchActions?.({ query, navigate }))
    .filter(Boolean);
}

function messageAnnotationNodes(plugins: RuntimePlugin[], message: Conversation["message"]) {
  const annotations = message.annotations || [];
  if (annotations.length === 0) return [];
  return (plugins as MessageAnnotationPlugin[])
    .map((plugin, index) => {
      const node = plugin.renderMessageAnnotations?.({ location: "message-list", message, annotations });
      return node ? <span key={`message-annotation-${index}`}>{node}</span> : null;
    })
    .filter(Boolean);
}

/**
 * MailView fetches one page of mailbox/all-mail conversations. It clears stale
 * rows when the URL changes, animates newly delivered messages on the first page,
 * and shows a folder-level sync clue when the selected mailbox is manual or off.
 */
export function MailView({
  userID,
  csrf,
  datePrefs,
  location,
  navigate,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  latestSyncRun,
  activeSyncRuns,
  mailGeneration,
  refreshChrome,
  messageSecurityPlugins = [],
  addToast
}: {
  userID: number;
  csrf: string;
  datePrefs: DatePrefs;
  location: LocationState;
  navigate: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  latestSyncRun: SyncRun | null;
  activeSyncRuns: SyncRun[];
  mailGeneration: number;
  refreshChrome: () => Promise<Bootstrap | null>;
  messageSecurityPlugins?: RuntimePlugin[];
  addToast: AddToast;
}) {
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [loading, setLoading] = useState(true);
  const [syncBusy, setSyncBusy] = useState(false);
  const [pullRefreshing, setPullRefreshing] = useState(false);
  const [manualRefreshGeneration, setManualRefreshGeneration] = useState(0);
  const loaded = useRef(false);
  const manualViewSyncKey = useRef("");
  const [error, setError] = useState("");
  const [showingSavedPage, setShowingSavedPage] = useState(false);
  const [hasPrev, setHasPrev] = useState(false);
  const [hasNext, setHasNext] = useState(false);
  const [newMessageIDs, setNewMessageIDs] = useState<Set<number>>(() => new Set());
  const previousPageIDs = useRef<Set<number>>(new Set());
  const previousListKey = useRef("");
  const newMessageTimer = useRef<number | null>(null);
  const route = mailRoute(location.path);
  const mailboxID = route.mailboxID;
  const page = route.page;
  const mailbox = mailboxes.find((item) => String(item.id) === mailboxID);
  const totalCount = mailbox ? mailbox.message_count : mailboxes.filter((item) => item.show_in_all_mail !== false).reduce((sum, item) => sum + item.message_count, 0);
  const refreshKey = `${mailGeneration}:${manualRefreshGeneration}:${mailboxRefreshKey(latestSyncRun, mailbox)}`;
  const listScopeKey = `${userID}:${mailboxID || "all"}`;
  const listKey = listScopeKey + ":" + page;
  const slideDirection = useListSlideDirection(listScopeKey, page);
  const cachedTransitionPage = previousListKey.current !== listKey ? api.cachedMail(userID, mailboxID, page) : null;
  const displayConversations = cachedTransitionPage?.conversations || conversations;
  const displayHasPrev = cachedTransitionPage?.has_prev ?? hasPrev;
  const displayHasNext = cachedTransitionPage?.has_next ?? hasNext;
  const listPending = (loading || previousListKey.current !== listKey) && !cachedTransitionPage;
  const listTransitionSpeed: SlideSpeed = cachedTransitionPage ? "fast" : listPending ? "slow" : "fast";
  const activeRun = mailboxActiveRun(mailbox, activeSyncRuns, latestSyncRun);
  const effectiveMode = mailbox ? effectiveMailboxSyncMode(mailbox, mailboxes) : "auto";
  const accountActiveRun = activeRun || (mailbox ? activeSyncRuns.find((run) =>
    run.status === "running" && run.account_id === mailbox.account_id
  ) || null : null);
  const showRecoveryEmptyState = Boolean(
    mailbox &&
    displayConversations.length === 0 &&
    mailbox.remote_message_count > 0 &&
    typeof mailbox.local_message_count === "number" &&
    mailbox.local_message_count < mailbox.remote_message_count &&
    (effectiveMode === "auto" || Boolean(accountActiveRun))
  );
  const syncAlreadyRunning = syncBusy || (mailbox ? Boolean(activeRun) : activeSyncRuns.length > 0);

  async function refreshByPull() {
    const startedAt = performance.now();
    setPullRefreshing(true);
    try {
      try {
        if (!syncAlreadyRunning && (!mailbox || effectiveMode !== "never")) {
          if (mailbox) await api.syncFolder(csrf, mailbox.id);
          else await api.syncAccount(csrf);
        }
      } catch (err) {
        // A sync may start between the chrome snapshot and this request. Its SSE
        // updates will still refresh the list, so a conflict is not a pull error.
        if (!(err instanceof ApiError && err.status === 409)) {
          addToast(`Refresh failed: ${messageFromError(err)}`, "error");
        }
      }
      setManualRefreshGeneration((current) => current + 1);
      await refreshChrome();
    } finally {
      const remaining = 450 - (performance.now() - startedAt);
      if (remaining > 0) await new Promise((resolve) => window.setTimeout(resolve, remaining));
      setPullRefreshing(false);
    }
  }

  const pullRefresh = usePullToRefresh<HTMLDivElement>({
    disabled: listPending || pullRefreshing || syncBusy,
    onRefresh: refreshByPull
  });
  const pullStyle = { "--pull-distance": `${pullRefresh.distance}px` } as CSSProperties;

  useEffect(() => {
    return () => {
      if (newMessageTimer.current !== null) window.clearTimeout(newMessageTimer.current);
    };
  }, []);

  // A manual folder is refreshed when the user enters its view. The ref
  // coalesces chrome/SSE rerenders, while a new component mount (browser
  // refresh) or navigating away and back permits one fresh request.
  useEffect(() => {
    if (!mailbox || effectiveMode !== "manual") {
      manualViewSyncKey.current = "";
      return;
    }
    const nextKey = `${userID}:${mailbox.id}`;
    if (manualViewSyncKey.current === nextKey) return;
    manualViewSyncKey.current = nextKey;
    let cancelled = false;
    api.syncFolder(csrf, mailbox.id).catch((err) => {
      // Older servers may still answer with a conflict if another sync won the
      // race. Its event stream will refresh this view, so that is not an error.
      if (err instanceof ApiError && err.status === 409) return;
      if (!cancelled) addToast(`Folder refresh failed: ${messageFromError(err)}`, "error");
    });
    return () => {
      cancelled = true;
    };
  }, [userID, mailbox?.id, effectiveMode, csrf, addToast]);

  // Route changes should feel immediate: clear the old page before the server
  // responds so the user is not looking at stale rows for another folder.
  useEffect(() => {
    let cancelled = false;
    const isNewList = previousListKey.current !== listKey;
    const canAnimateNewMail = page === 1 && loaded.current && !isNewList && Boolean(refreshKey) && Boolean(latestSyncRun?.new_messages);
    if (isNewList || !loaded.current) {
      const cached = api.cachedMail(userID, mailboxID, page);
      if (cached) {
        previousPageIDs.current = new Set(cached.conversations.map((conversation) => conversation.message.id));
        previousListKey.current = listKey;
        setConversations(cached.conversations);
        setHasPrev(cached.has_prev);
        setHasNext(cached.has_next);
        setLoading(false);
        setShowingSavedPage(false);
      } else {
        setLoading(true);
        setConversations([]);
        setHasPrev(false);
        setHasNext(false);
        setShowingSavedPage(false);
      }
    }
    setError("");
    api
      .mail(userID, mailboxID, page)
      .then((data) => {
        if (cancelled) return;
        const nextIDs = new Set(data.conversations.map((conversation) => conversation.message.id));
        if (canAnimateNewMail) {
          const appeared = data.conversations
            .map((conversation) => conversation.message.id)
            .filter((id) => !previousPageIDs.current.has(id));
          if (appeared.length > 0) {
            setNewMessageIDs(new Set(appeared));
            if (newMessageTimer.current !== null) window.clearTimeout(newMessageTimer.current);
            newMessageTimer.current = window.setTimeout(() => setNewMessageIDs(new Set()), 1200);
          }
        } else {
          setNewMessageIDs(new Set());
        }
        previousPageIDs.current = nextIDs;
        previousListKey.current = listKey;
        setConversations(data.conversations);
        setHasPrev(data.has_prev);
        setHasNext(data.has_next);
        setShowingSavedPage(false);
        if (data.has_next) api.prefetchMail(userID, mailboxID, page + 1);
        if (data.has_prev && page > 1) api.prefetchMail(userID, mailboxID, page - 1);
      })
      .catch((err) => {
        if (!cancelled) {
          const cached = api.cachedMail(userID, mailboxID, page);
          previousListKey.current = listKey;
          if (cached) {
            previousPageIDs.current = new Set(cached.conversations.map((conversation) => conversation.message.id));
            setConversations(cached.conversations);
            setHasPrev(cached.has_prev);
            setHasNext(cached.has_next);
            setShowingSavedPage(true);
            setError(`Showing saved mail. Refresh failed: ${messageFromError(err)}`);
          } else {
            previousPageIDs.current = new Set();
            setConversations([]);
            setHasPrev(false);
            setHasNext(false);
            setShowingSavedPage(false);
            setError(messageFromError(err));
          }
        }
      })
      .finally(() => {
        if (!cancelled) {
          loaded.current = true;
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [userID, mailboxID, page, refreshKey, listKey, latestSyncRun?.new_messages]);

  const pageURL = (nextPage: number) => mailURL(mailboxID, nextPage);

  function updateStarred(messageID: number, starredMessageID: number, starred: boolean) {
    setConversations((current) => current.map((conversation) => {
      if (conversation.message.id !== messageID && conversation.starred_message_id !== starredMessageID) return conversation;
      return {
        ...conversation,
        starred_message_id: starred ? starredMessageID : conversation.message.id,
        message: { ...conversation.message, is_starred: starred }
      };
    }));
  }

  function updateReadStates(states: ConversationReadState[]) {
    const readByID = new Map(states.map((state) => [state.id, state.read]));
    setConversations((current) => current.map((conversation) => {
      const read = readByID.get(conversation.message.id);
      if (read === undefined) return conversation;
      return { ...conversation, is_read: read, message: { ...conversation.message, is_read: read } };
    }));
  }

  function removeMovedConversations(messageIDs: number[]) {
    const moved = new Set(messageIDs);
    setConversations((current) => current.filter((conversation) =>
      !conversationTransferMessageIDs(conversation).some((id) => moved.has(id))
    ));
  }

  async function startFolderSync() {
    if (!mailbox) return;
    if (effectiveMode === "never") {
      addToast(`${mailbox.name} is set to Never. Change the folder sync mode before syncing.`, "error");
      return;
    }
    setSyncBusy(true);
    try {
      await api.syncFolder(csrf, mailbox.id);
      addToast(`${mailbox.name} sync started.`);
      await refreshChrome();
    } catch (err) {
      addToast(`Sync failed: ${messageFromError(err)}`, "error");
    } finally {
      setSyncBusy(false);
    }
  }

  return (
    <>
      <ListHeader
        title={mailbox?.name || "All Mail"}
        titleClassName="mailbox-title"
        pager={{
          page,
          pageSize: mailPageSize,
          itemCount: listPending ? 0 : displayConversations.length,
          total: totalCount,
          hasPrev: listPending ? false : displayHasPrev,
          hasNext: listPending ? false : displayHasNext,
          pageURL,
          navigate,
          ariaLabel: "Mailbox pagination",
          loading: listPending
        }}
      />
      <div
        className={`mail-pull-refresh${pullRefresh.distance > 0 ? " pulling" : ""}${pullRefresh.ready ? " ready" : ""}${pullRefreshing ? " refreshing" : ""}`}
        ref={pullRefresh.targetRef}
        style={pullStyle}
      >
        <div
          className="pull-refresh-indicator"
          role="status"
          aria-live="polite"
          aria-label={pullRefreshing ? "Refreshing mail" : pullRefresh.ready ? "Release to refresh mail" : pullRefresh.distance > 0 ? "Pull to refresh mail" : undefined}
        >
          <Icon name="sync" />
          {pullRefreshing ? <span>Refreshing mail</span> : null}
        </div>
        {mailbox ? (
          <FolderSyncNotice
            mailbox={mailbox}
            effectiveMode={effectiveMode}
            activeRun={activeRun}
            busy={syncBusy}
            onSync={startFolderSync}
          />
        ) : null}
        {error ? <div className={showingSavedPage ? "mail-cache-warning" : "error"} role="status">{error}</div> : null}
        {!error || showingSavedPage ? (
          <SlidingMessageListStage stageKey={listKey} direction={slideDirection} pending={listPending} speed={listTransitionSpeed}>
            {listPending ? (
              <div className="mail-list-loading" role="status" aria-label="Refreshing mail" aria-busy="true"><span /></div>
            ) : (
              <MessageList
                csrf={csrf}
                conversations={displayConversations}
                hiddenMessageIDs={hiddenMessageIDs}
                mailboxes={mailboxes}
                currentMailboxID={mailbox?.id || 0}
                swipePreferences={swipePreferences}
                highlightMessageIDs={newMessageIDs}
                showRecipients={mailbox?.role === "sent" || mailbox?.role === "drafts"}
                openAsDraft={mailbox?.role === "drafts"}
                datePrefs={datePrefs}
                returnURL={mailURL(mailboxID, page)}
                navigate={navigate}
                messageSecurityPlugins={messageSecurityPlugins}
                addToast={addToast}
                onStarredChange={updateStarred}
                onReadStatesChange={updateReadStates}
                onMessagesMoved={removeMovedConversations}
                emptyState={showRecoveryEmptyState && mailbox ? (
                  <MailboxRecoveryEmptyState mailbox={mailbox} activeRun={accountActiveRun} />
                ) : undefined}
              />
            )}
          </SlidingMessageListStage>
        ) : null}
      </div>
    </>
  );
}

/** SnoozedView reuses the normal conversation list for active local reminders. */
export function SnoozedView({
  csrf,
  datePrefs,
  location,
  navigate,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  mailGeneration,
  messageSecurityPlugins = [],
  addToast
}: {
  csrf: string;
  datePrefs: DatePrefs;
  location: LocationState;
  navigate: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  mailGeneration: number;
  messageSecurityPlugins?: RuntimePlugin[];
  addToast: AddToast;
}) {
  const page = Math.max(1, Number.parseInt(new URLSearchParams(location.search).get("page") || "1", 10) || 1);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [hasPrev, setHasPrev] = useState(false);
  const [hasNext, setHasNext] = useState(false);
  const [refreshGeneration, setRefreshGeneration] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const pullRefresh = usePullToRefresh<HTMLDivElement>({
    disabled: loading || refreshing,
    onRefresh: async () => {
      setRefreshing(true);
      setRefreshGeneration((current) => current + 1);
      await new Promise((resolve) => window.setTimeout(resolve, 350));
      setRefreshing(false);
    }
  });
  const pullStyle = { "--pull-distance": `${pullRefresh.distance}px` } as CSSProperties;

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError("");
    api.snoozes(page)
      .then((data) => {
        if (cancelled) return;
        setConversations(data.conversations);
        setHasPrev(data.has_prev);
        setHasNext(data.has_next);
      })
      .catch((err) => {
        if (!cancelled) setError(messageFromError(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [page, mailGeneration, refreshGeneration]);

  function updateStarred(messageID: number, starredMessageID: number, starred: boolean) {
    setConversations((current) => current.map((conversation) => {
      if (conversation.message.id !== messageID && conversation.starred_message_id !== starredMessageID) return conversation;
      return { ...conversation, starred_message_id: starred ? starredMessageID : conversation.message.id, message: { ...conversation.message, is_starred: starred } };
    }));
  }

  function updateReadStates(states: ConversationReadState[]) {
    const readByID = new Map(states.map((state) => [state.id, state.read]));
    setConversations((current) => current.map((conversation) => {
      const read = readByID.get(conversation.message.id);
      return read === undefined ? conversation : { ...conversation, is_read: read, message: { ...conversation.message, is_read: read } };
    }));
  }

  function removeMovedConversations(messageIDs: number[]) {
    const moved = new Set(messageIDs);
    setConversations((current) => current.filter((conversation) =>
      !conversationTransferMessageIDs(conversation).some((id) => moved.has(id))
    ));
  }

  const pageURL = (nextPage: number) => `/snoozes${nextPage > 1 ? `?page=${nextPage}` : ""}`;
  return (
    <>
      <ListHeader
        title="Snoozed"
        pager={{ page, pageSize: mailPageSize, itemCount: loading ? 0 : conversations.length, hasPrev, hasNext, pageURL, navigate, ariaLabel: "Snoozed pagination", loading }}
      />
    <div
    className={`mail-pull-refresh${pullRefresh.distance > 0 ? " pulling" : ""}${pullRefresh.ready ? " ready" : ""}${refreshing ? " refreshing" : ""}`}
    ref={pullRefresh.targetRef}
    style={pullStyle}
    >
    <div className="pull-refresh-indicator" role="status" aria-live="polite">
      <Icon name="sync" />
      {refreshing ? <span>Refreshing snoozed</span> : null}
    </div>
    {error ? <div className="error">{error}</div> : null}
    {!error ? <div className="message-list-pane">
      {loading ? <div className="mail-list-loading" role="status" aria-label="Refreshing snoozed mail" aria-busy="true"><span /></div> : (
            <MessageList
              csrf={csrf}
              conversations={conversations}
              hiddenMessageIDs={hiddenMessageIDs}
              mailboxes={mailboxes}
              swipePreferences={swipePreferences}
              datePrefs={datePrefs}
              returnURL={routeWithSearch(location.path, location.search)}
              navigate={navigate}
              messageSecurityPlugins={messageSecurityPlugins}
              addToast={addToast}
              onStarredChange={updateStarred}
              onReadStatesChange={updateReadStates}
              onMessagesMoved={removeMovedConversations}
              snoozedView
            />
      )}
    </div> : null}
    </div>
    </>
  );
}

// FolderSyncNotice is shown only when the selected folder is known to be
// excluded from automatic sync or behind the remote mailbox.
function FolderSyncNotice({
  mailbox,
  effectiveMode,
  activeRun,
  busy,
  onSync
}: {
  mailbox: Mailbox;
  effectiveMode: string;
  activeRun: SyncRun | null;
  busy: boolean;
  onSync: () => void;
}) {
  const syncOff = effectiveMode === "never";
  const needsManualSync = effectiveMode === "manual" && mailboxNeedsSync(mailbox) && !activeRun;
  if (!syncOff && !needsManualSync) return null;

  const title = syncOff ? "Folder sync is off" : "Folder is not fully synced";
  const detail = syncOff
    ? "This folder is excluded from sync. Change its sync mode in folder settings before mirroring it."
    : "This manual-sync folder is behind the remote mailbox. Sync it to mirror the latest messages.";
  const buttonLabel = busy ? "Starting" : "Sync folder";

  return (
    <section className="folder-sync-notice" aria-live="polite">
      <Icon name="report" />
      <div className="folder-sync-copy">
        <strong>{title}</strong>
        <span>{detail}</span>
      </div>
      {!syncOff ? (
        <button className="secondary" type="button" disabled={busy} onClick={onSync}>
          <Icon name="sync" />
          {buttonLabel}
        </button>
      ) : null}
    </section>
  );
}

function MailboxRecoveryEmptyState({
  mailbox,
  activeRun
}: {
  mailbox: Mailbox;
  activeRun: SyncRun | null;
}) {
  const localCount = Math.max(0, mailbox.local_message_count || 0);
  const remoteCount = Math.max(0, mailbox.remote_message_count || 0);
  const runMailbox = activeRun?.current_mailbox.trim() || "";
  const runMatchesMailbox = Boolean(activeRun) && runMailbox.toLowerCase() === mailbox.name.trim().toLowerCase();
  const total = Math.max(0, activeRun?.messages_total || 0);
  const seen = Math.max(0, activeRun?.messages_seen || 0);
  const progress = total > 0 ? Math.min(100, Math.round((seen / total) * 100)) : 0;
  const title = runMatchesMailbox
    ? "Loading this folder"
    : activeRun
      ? "Account sync is still running"
      : "This folder has not finished loading";
  const activity = activeRun
    ? total > 0
      ? `${runMatchesMailbox ? "Folder sync" : `Syncing ${runMailbox || "this account"}`}: ${Math.min(seen, total).toLocaleString()} of ${total.toLocaleString()} checked.`
      : runMatchesMailbox
        ? "Checking this folder for messages now."
        : `Currently syncing ${runMailbox || "this account"}.`
    : "Waiting for this folder's next synchronization.";

  return (
    <section
      className={`mailbox-recovery-empty${activeRun ? " running" : ""}`}
      role="status"
      aria-live="polite"
      aria-busy={Boolean(activeRun)}
    >
      <Icon name={activeRun ? "sync" : "report"} />
      <div className="folder-sync-copy">
        <strong>{title}</strong>
        <span>
          The mail server reports {remoteCount.toLocaleString()} messages; {localCount.toLocaleString()} are available in Rolltop so far.
        </span>
        <span>{activity}</span>
        {activeRun && total > 0 ? (
          <div
            className="folder-sync-progress"
            role="progressbar"
            aria-label={`${runMailbox || mailbox.name} sync progress`}
            aria-valuemin={0}
            aria-valuemax={total}
            aria-valuenow={Math.min(seen, total)}
          >
            <div style={{ width: `${progress}%` }} />
          </div>
        ) : null}
      </div>
    </section>
  );
}


function activeSearchMaintenanceRun(runs: SyncRun[]): SyncRun | null {
  return runs.find((run) => {
    const subject = (run.latest_new_subject || "").toLowerCase();
    return subject === "purging full-text index" ||
      subject === "purging local references and full-text index" ||
      subject === "repairing full-text index" ||
      subject.includes("search index repair");
  }) || null;
}

function SearchMaintenanceNotice({ run }: { run: SyncRun }) {
  const total = Math.max(0, run.messages_total || 0);
  const seen = Math.max(0, run.messages_seen || 0);
  const done = total > 0 ? Math.min(seen, total) : seen;
  const remaining = total > 0 ? Math.max(0, total - done) : 0;
  const label = run.latest_new_subject || "Full-text indexing";
  const scope = run.current_mailbox ? ` in ${run.current_mailbox}` : "";
  const progress = total > 0
    ? `${done.toLocaleString()} of ${total.toLocaleString()} messages checked`
    : done > 0 ? `${done.toLocaleString()} messages checked` : "Index work is running";
  const remainingText = remaining > 0 ? `, ${remaining.toLocaleString()} remaining` : "";

  return (
    <section className="folder-sync-notice search-maintenance-notice running" aria-live="polite">
      <Icon name="report" />
      <div className="folder-sync-copy">
        <strong>Search may be slow</strong>
        <span>{label}{scope}. {progress}{remainingText}.</span>
      </div>
    </section>
  );
}

/**
 * SearchView is always best-match search. The URL carries the query and page so
 * opening a result can preserve a precise back target to the same result page.
 */
export function SearchView({
  csrf,
  location,
  navigate,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  datePrefs,
  activeSyncRuns,
  messageSecurityPlugins = [],
  searchActionPlugins = [],
  addToast
}: {
  csrf: string;
  location: LocationState;
  navigate: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  datePrefs: DatePrefs;
  activeSyncRuns: SyncRun[];
  messageSecurityPlugins?: RuntimePlugin[];
  searchActionPlugins?: RuntimePlugin[];
  addToast: AddToast;
}) {
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [hasPrev, setHasPrev] = useState(false);
  const [hasNext, setHasNext] = useState(false);
  const loadedKey = useRef("");
  const route = searchRoute(location.path);
  const query = route.query;
  const page = route.page;
  const searchKey = query + ":best:" + page;
  const slideDirection = useListSlideDirection("search:" + query, page);
  const listPending = loading || loadedKey.current !== searchKey;

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setConversations([]);
    setHasPrev(false);
    setHasNext(false);
    setError("");
    api
      .search(query, page)
      .then((data) => {
        if (cancelled) return;
        loadedKey.current = searchKey;
        setConversations(data.conversations);
        setHasPrev(data.has_prev);
        setHasNext(data.has_next);
        if (data.has_next) api.prefetchSearch(query, page + 1);
      })
      .catch((err) => {
        if (!cancelled) {
          loadedKey.current = searchKey;
          setConversations([]);
          setHasPrev(false);
          setHasNext(false);
          setError(messageFromError(err));
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [query, page, searchKey]);

  const pageURL = (nextPage: number) => searchURL(query, nextPage);
  const returnURL = routeWithSearch(location.path, location.search);
  const maintenanceRun = activeSearchMaintenanceRun(activeSyncRuns);
  const pluginSearchActions = searchActionNodes(searchActionPlugins, query, navigate);

  function updateStarred(messageID: number, starredMessageID: number, starred: boolean) {
    setConversations((current) => current.map((conversation) => {
      if (conversation.message.id !== messageID && conversation.starred_message_id !== starredMessageID) return conversation;
      return {
        ...conversation,
        starred_message_id: starred ? starredMessageID : conversation.message.id,
        message: { ...conversation.message, is_starred: starred }
      };
    }));
  }

  function updateReadStates(states: ConversationReadState[]) {
    const readByID = new Map(states.map((state) => [state.id, state.read]));
    setConversations((current) => current.map((conversation) => {
      const read = readByID.get(conversation.message.id);
      if (read === undefined) return conversation;
      return { ...conversation, is_read: read, message: { ...conversation.message, is_read: read } };
    }));
  }

  function removeMovedConversations(messageIDs: number[]) {
    const moved = new Set(messageIDs);
    setConversations((current) => current.filter((conversation) =>
      !conversationTransferMessageIDs(conversation).some((id) => moved.has(id))
    ));
  }

  return (
    <>
      <ListHeader
        title="Search"
        pager={{
          page,
          pageSize: mailPageSize,
          itemCount: listPending ? 0 : conversations.length,
          hasPrev: listPending ? false : hasPrev,
          hasNext: listPending ? false : hasNext,
          pageURL,
          navigate,
          ariaLabel: "Search pagination",
          loading: listPending
        }}
      />
      {query ? (
        <div className="search-result-actions">
          <div className="muted">Results for <strong>{query}</strong></div>
          {pluginSearchActions}
        </div>
      ) : null}
      {maintenanceRun ? <SearchMaintenanceNotice run={maintenanceRun} /> : null}
      {error ? <div className="error">{error}</div> : null}
      {!error ? (
        <SlidingMessageListStage stageKey={searchKey} direction={slideDirection} pending={listPending} speed={listPending ? "slow" : "fast"}>
          {listPending ? (
            <div className="mail-list-loading" role="status" aria-label="Searching mail" aria-busy="true"><span /></div>
          ) : (
            <MessageList
              csrf={csrf}
              conversations={conversations}
              hiddenMessageIDs={hiddenMessageIDs}
              mailboxes={mailboxes}
              swipePreferences={swipePreferences}
              navigate={navigate}
              searchQuery={query}
              datePrefs={datePrefs}
              returnURL={returnURL}
              addToast={addToast}
              messageSecurityPlugins={messageSecurityPlugins}
              onStarredChange={updateStarred}
              onReadStatesChange={updateReadStates}
              onMessagesMoved={removeMovedConversations}
            />
          )}
        </SlidingMessageListStage>
      ) : null}
    </>
  );
}

type SlideDirection = "left" | "right" | "none";
type SlideSpeed = "fast" | "slow";

type OutgoingListPane = {
  key: string;
  child: ReactNode;
  direction: Exclude<SlideDirection, "none">;
};

function useListSlideDirection(scopeKey: string, page: number): SlideDirection {
  const previous = useRef({ scopeKey, page });
  const direction = useRef<SlideDirection>("none");
  if (previous.current.scopeKey !== scopeKey || previous.current.page !== page) {
    direction.current = previous.current.scopeKey === scopeKey && page !== previous.current.page
      ? page > previous.current.page ? "left" : "right"
      : "none";
    previous.current = { scopeKey, page };
  }
  return direction.current;
}

function SlidingMessageListStage({
  stageKey,
  direction,
  pending,
  speed,
  children
}: {
  stageKey: string;
  direction: SlideDirection;
  pending: boolean;
  speed: SlideSpeed;
  children: ReactNode;
}) {
  const lastPane = useRef({ key: stageKey, child: children });
  const measuredHeight = useRef(0);
  const currentPane = useRef<HTMLDivElement | null>(null);
  const [outgoing, setOutgoing] = useState<OutgoingListPane | null>(null);
  const [lockedHeight, setLockedHeight] = useState<number | null>(null);

  useLayoutEffect(() => {
    if (lastPane.current.key === stageKey) return;
    const previous = lastPane.current;
    if (measuredHeight.current > 0) setLockedHeight(measuredHeight.current);
    if (direction !== "none") {
      setOutgoing({ key: previous.key, child: previous.child, direction });
      const timer = window.setTimeout(() => {
        setOutgoing((current) => current?.key === previous.key ? null : current);
      }, speed === "slow" ? 640 : 140);
      lastPane.current = { key: stageKey, child: children };
      return () => window.clearTimeout(timer);
    }
    lastPane.current = { key: stageKey, child: children };
    setOutgoing(null);
  }, [stageKey, direction, speed, children]);

  useLayoutEffect(() => {
    lastPane.current = { key: stageKey, child: children };
    if (!pending && currentPane.current) {
      measuredHeight.current = currentPane.current.offsetHeight;
    }
  });

  useLayoutEffect(() => {
    if (!pending && !outgoing) setLockedHeight(null);
  }, [pending, outgoing]);

  const stageStyle = lockedHeight ? { minHeight: `${lockedHeight}px` } : undefined;
  if (outgoing) {
    const incomingPane = (
      <div className="message-list-pane incoming" key={stageKey} ref={currentPane}>
        {children}
      </div>
    );
    const outgoingPane = (
      <div className="message-list-pane outgoing" key={`out-${outgoing.key}`}>
        {outgoing.child}
      </div>
    );
    return (
      <div className={`message-list-stage speed-${speed}`} style={stageStyle}>
        <div
          className={`message-list-track slide-${outgoing.direction}`}
          onAnimationEnd={() => setOutgoing((current) => current?.key === outgoing.key ? null : current)}
        >
          {outgoing.direction === "right" ? incomingPane : outgoingPane}
          {outgoing.direction === "right" ? outgoingPane : incomingPane}
        </div>
      </div>
    );
  }
  return (
    <div className={`message-list-stage speed-${speed}`} style={stageStyle}>
      <div className="message-list-pane" key={stageKey} ref={currentPane}>
        {children}
      </div>
    </div>
  );
}

function messageDragPreview(conversations: Conversation[], ids: number[]) {
  if (typeof document === "undefined" || ids.length === 0) return null;
  const idSet = new Set(ids);
  const rows = conversations.filter((conversation) => conversationTransferMessageIDs(conversation).some((id) => idSet.has(id)));
  const preview = document.createElement("div");
  preview.className = "message-drag-preview";
  preview.setAttribute("aria-hidden", "true");
  const count = ids.length;
  const title = document.createElement("div");
  title.className = "message-drag-preview-count";
  title.textContent = messageCountLabel(count);
  preview.appendChild(title);
  rows.slice(0, 4).forEach((conversation) => {
    const line = document.createElement("div");
    line.className = "message-drag-preview-row";
    const sender = conversation.participants || conversation.message.from_addr || "Unknown sender";
    const subject = conversation.message.subject || "(no subject)";
    line.textContent = `${sender} - ${subject}`;
    preview.appendChild(line);
  });
  if (count > rows.length || count > 4) {
    const more = document.createElement("div");
    more.className = "message-drag-preview-more";
    more.textContent = `+${Math.max(0, count - Math.min(rows.length, 4)).toLocaleString()} more`;
    preview.appendChild(more);
  }
  document.body.appendChild(preview);
  return preview;
}

function uniquePositiveIDs(ids: number[]): number[] {
  return Array.from(new Set(ids.filter((id) => Number.isFinite(id) && id > 0)));
}

function conversationTransferMessageIDs(conversation: Conversation): number[] {
  const ids = conversation.message_ids && conversation.message_ids.length > 0 ? conversation.message_ids : [conversation.message.id];
  return uniquePositiveIDs(ids);
}

function conversationTransferAccountIDs(conversation: Conversation): number[] {
  const ids = conversation.message_account_ids && conversation.message_account_ids.length > 0
    ? conversation.message_account_ids
    : [conversation.message.account_id];
  return uniquePositiveIDs(ids);
}

type ConversationReadState = {
  id: number;
  read: boolean;
};

/** TrashMoveGroup collects the selected rows headed for one Trash mailbox. */
type TrashMoveGroup = {
  target: Mailbox;
  messageIDs: number[];
  items: { rowID: number; messageIDs: number[] }[];
};

// The backend queues a background run for bulk moves above 5 message IDs and
// stops reporting per-message progress; smaller moves run one message at a
// time so a partial failure restores only the rows that did not move.
const inlineMoveMessageLimit = 5;

// Keepalive request bodies share a 64 KiB browser quota; a 1000-ID move body
// is roughly 10 KiB, so background commits dispatch at most this many chunks —
// anything beyond the quota would be rejected by the browser anyway.
const keepaliveMoveChunkBudget = 6;

const messageSwipeMaxDistance = 112;
const messageSwipeCommitDistance = 68;
const messageSwipeCommitHoldMS = 170;
const messageSwipeSettleMS = 210;
const messageSwipeExitMS = 320;

type MessageSwipeState = {
  id: number;
  deltaX: number;
  visualDeltaX: number;
  direction: "start" | "end";
  phase: "tracking" | "committing" | "settling" | "exiting";
  committed: boolean;
  rowHeight?: number;
};

function messageSwipeAffordanceStyle(state: MessageSwipeState): CSSProperties | undefined {
  const deltaX = state.visualDeltaX;
  const distance = Math.abs(deltaX);
  if (distance === 0) return undefined;
  const progress = Math.min(distance / messageSwipeCommitDistance, 1);
  const overshoot = Math.min(Math.max(distance - messageSwipeCommitDistance, 0) / (messageSwipeMaxDistance - messageSwipeCommitDistance), 1);
  const shift = 12 * (1 - progress);
  const iconScale = progress < 1 ? .76 + (.24 * progress) : 1.08 - (.08 * overshoot);
  const labelProgress = Math.min(Math.max((distance - 18) / (messageSwipeCommitDistance - 18), 0), 1);
  const style = {
    "--swipe-action-content-opacity": (.28 + (.72 * progress)).toFixed(3),
    "--swipe-action-icon-scale": iconScale.toFixed(3),
    "--swipe-action-label-opacity": labelProgress.toFixed(3),
    "--swipe-action-start-shift": `-${shift.toFixed(1)}px`,
    "--swipe-action-end-shift": `${shift.toFixed(1)}px`
  } as CSSProperties & Record<string, string>;
  if (state.rowHeight) style["--swipe-row-height"] = `${state.rowHeight}px`;
  return style;
}

// MessageList is shared by mailbox and search pages. It owns local row selection,
// shift-select ranges, drag payloads, optimistic star updates, and message links.
function MessageList({
  csrf,
  conversations,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  highlightMessageIDs,
  showRecipients = false,
  openAsDraft = false,
  searchQuery = "",
  datePrefs,
  returnURL = "",
  navigate,
  messageSecurityPlugins = [],
  addToast,
  onStarredChange,
  onReadStatesChange,
  onMessagesMoved,
  snoozedView = false,
  currentMailboxID = 0,
  emptyState
}: {
  csrf: string;
  conversations: Conversation[];
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  highlightMessageIDs?: Set<number>;
  showRecipients?: boolean;
  openAsDraft?: boolean;
  searchQuery?: string;
  datePrefs: DatePrefs;
  returnURL?: string;
  navigate: (url: string) => void;
  messageSecurityPlugins?: RuntimePlugin[];
  addToast: AddToast;
  onStarredChange: (messageID: number, starredMessageID: number, starred: boolean) => void;
  onReadStatesChange: (states: ConversationReadState[]) => void;
  onMessagesMoved: (messageIDs: number[]) => void;
  snoozedView?: boolean;
  /** The mailbox this list is showing, so Delete can skip rows already in it. */
  currentMailboxID?: number;
  emptyState?: ReactNode;
}) {
  const [selectedIDs, setSelectedIDs] = useState<Set<number>>(() => new Set());
  const [dismissedIDs, setDismissedIDs] = useState<Set<number>>(() => new Set());
  const [readStateBusy, setReadStateBusy] = useState(false);
  const [snoozeBusy, setSnoozeBusy] = useState(false);
  // A counter rather than a boolean: deferred delete commits can overlap when
  // a second selection is deleted during the first commit's undo window.
  const [trashOps, setTrashOps] = useState(0);
  const [swipeActionBusy, setSwipeActionBusy] = useState(false);
  const [pendingSwipeMoveIDs, setPendingSwipeMoveIDs] = useState<Set<number>>(() => new Set());
  const [pendingSwipeSnoozeIDs, setPendingSwipeSnoozeIDs] = useState<Set<number>>(() => new Set());
  const [pendingSwipeReadStates, setPendingSwipeReadStates] = useState<Map<number, boolean>>(() => new Map());
  const [swipeState, setSwipeState] = useState<MessageSwipeState | null>(null);
  const [keyboardIndex, setKeyboardIndex] = useState<number | null>(null);
  const selectionAnchorID = useRef<number | null>(null);
  const moveOutTimers = useRef<Map<number, number>>(new Map());
  const swipeCompletionTimer = useRef<number | null>(null);
  const pendingSwipeActionIDs = useRef<Set<number>>(new Set());
  const rowRefs = useRef<Map<number, HTMLDivElement>>(new Map());
  const swipeDismissTimers = useRef<Map<number, number>>(new Map());
  const keyboardIndexRef = useRef<number | null>(null);
  const swipeSession = useRef<{ id: number; startX: number; startY: number; lastX: number; lastY: number; active: boolean; blocked: boolean } | null>(null);
  const suppressRowClickUntil = useRef(0);
  // selectionBusy gates every bulk row mutation (toolbar buttons, swipes, drags)
  // so concurrent moves cannot race each other on the same rows.
  const selectionBusy = readStateBusy || snoozeBusy || swipeActionBusy || trashOps > 0;
  const visible = conversations
    .filter((conversation) => !dismissedIDs.has(conversation.message.id))
    .map((conversation) => {
      const pendingRead = pendingSwipeReadStates.get(conversation.message.id);
      if (pendingRead === undefined || pendingRead === conversation.is_read) return conversation;
      return { ...conversation, is_read: pendingRead, message: { ...conversation.message, is_read: pendingRead } };
    });
  const visibleKey = visible.map((conversation) => conversation.message.id).join(",");
  const sourceKey = conversations.map((conversation) => conversation.message.id).join(",");
  const hiddenKey = Array.from(hiddenMessageIDs).sort((a, b) => a - b).join(",");
  const pendingSwipeMoveKey = Array.from(pendingSwipeMoveIDs).sort((a, b) => a - b).join(",");
  const pendingSwipeSnoozeKey = Array.from(pendingSwipeSnoozeIDs).sort((a, b) => a - b).join(",");
  const nativeTouchDrag = androidNativeAvailable();
  const effectiveSwipePreferences = swipePreferences || defaultSwipePreferences();
  const leftSwipePresentation = swipeActionPresentation(effectiveSwipePreferences.left_action);
  const rightSwipePresentation = swipeActionPresentation(effectiveSwipePreferences.right_action);
  const selectedDragItems = selectedIDs.size > 0 ? visible.filter((conversation) => selectedIDs.has(conversation.message.id)) : [];
  const selectedDragMessageIDs = uniquePositiveIDs(selectedDragItems.flatMap(conversationTransferMessageIDs));
  const selectedDragAccountIDs = uniquePositiveIDs(selectedDragItems.flatMap(conversationTransferAccountIDs));

  keyboardIndexRef.current = keyboardIndex;

  useEffect(() => {
    return () => {
      moveOutTimers.current.forEach((timer) => window.clearTimeout(timer));
      moveOutTimers.current.clear();
      swipeDismissTimers.current.forEach((timer) => window.clearTimeout(timer));
      swipeDismissTimers.current.clear();
      if (swipeCompletionTimer.current !== null) window.clearTimeout(swipeCompletionTimer.current);
    };
  }, []);

  useEffect(() => {
    const sourceIDs = new Set(conversations.map((conversation) => conversation.message.id));
    const sourceMessageIDs = new Set(conversations.flatMap(conversationTransferMessageIDs));
    setDismissedIDs((current) => {
      const next = new Set<number>();
      current.forEach((id) => {
        if (sourceMessageIDs.has(id) && (hiddenMessageIDs.has(id) || pendingSwipeMoveIDs.has(id) || pendingSwipeSnoozeIDs.has(id))) next.add(id);
      });
      return next.size === current.size ? current : next;
    });
    setPendingSwipeMoveIDs((current) => {
      const next = new Set(Array.from(current).filter((id) => sourceMessageIDs.has(id)));
      return next.size === current.size ? current : next;
    });
    setPendingSwipeSnoozeIDs((current) => {
      const next = new Set(Array.from(current).filter((id) => sourceMessageIDs.has(id)));
      return next.size === current.size ? current : next;
    });
    sourceIDs.forEach((id) => {
      if (hiddenMessageIDs.has(id)) {
        if (!moveOutTimers.current.has(id)) {
          const timer = window.setTimeout(() => {
            moveOutTimers.current.delete(id);
            setDismissedIDs((current) => {
              const next = new Set(current);
              next.add(id);
              return next;
            });
          }, 230);
          moveOutTimers.current.set(id, timer);
        }
      } else {
        const timer = moveOutTimers.current.get(id);
        if (timer !== undefined) {
          window.clearTimeout(timer);
          moveOutTimers.current.delete(id);
        }
      }
    });
  }, [conversations, hiddenKey, pendingSwipeMoveKey, pendingSwipeSnoozeKey, sourceKey, hiddenMessageIDs, pendingSwipeMoveIDs, pendingSwipeSnoozeIDs]);

  useEffect(() => {
    const ids = new Set(visible.map((conversation) => conversation.message.id));
    setSelectedIDs((current) => {
      const next = new Set(Array.from(current).filter((id) => ids.has(id)));
      return next.size === current.size ? current : next;
    });
    if (selectionAnchorID.current !== null && !ids.has(selectionAnchorID.current)) {
      selectionAnchorID.current = null;
    }
    if (keyboardIndexRef.current !== null && keyboardIndexRef.current >= visible.length) {
      keyboardIndexRef.current = null;
      setKeyboardIndex(null);
    }
  }, [visibleKey]);

  useEffect(() => {
    function handleListShortcut(event: globalThis.KeyboardEvent) {
      if (event.shiftKey || shouldIgnoreMailShortcut(event) || visible.length === 0) return;
      const key = event.key.toLowerCase();
      if (key !== "j" && key !== "k" && key !== "x") return;
      event.preventDefault();
      const focusedRow = document.activeElement instanceof Element
        ? document.activeElement.closest<HTMLElement>("[data-rolltop-list-index]")
        : null;
      const focusedIndex = focusedRow ? Number.parseInt(focusedRow.dataset.rolltopListIndex || "", 10) : NaN;
      const currentIndex = Number.isFinite(focusedIndex) ? focusedIndex : keyboardIndexRef.current;
      const nextIndex = key === "j"
        ? currentIndex === null ? 0 : Math.min(visible.length - 1, currentIndex + 1)
        : key === "k"
          ? currentIndex === null ? visible.length - 1 : Math.max(0, currentIndex - 1)
          : currentIndex === null ? 0 : currentIndex;
      keyboardIndexRef.current = nextIndex;
      setKeyboardIndex(nextIndex);
      const messageID = visible[nextIndex].message.id;
      window.requestAnimationFrame(() => {
        const row = rowRefs.current.get(messageID);
        row?.focus({ preventScroll: true });
        row?.scrollIntoView({ block: "nearest" });
      });
      if (key === "x" && !event.repeat) {
        setSelectedIDs((current) => {
          const next = new Set(current);
          if (next.has(messageID)) next.delete(messageID);
          else next.add(messageID);
          return next;
        });
        selectionAnchorID.current = messageID;
      }
    }
    window.addEventListener("keydown", handleListShortcut);
    return () => window.removeEventListener("keydown", handleListShortcut);
  }, [visibleKey]);

  function selectedDragConversations(conversation: Conversation): Conversation[] {
    if (!selectedIDs.has(conversation.message.id)) return [conversation];
    const selected = visible.filter((item) => selectedIDs.has(item.message.id));
    return selected.length > 0 ? selected : [conversation];
  }

  function startMessageDrag(event: DragEvent<HTMLDivElement>, conversation: Conversation) {
    const selected = selectedDragConversations(conversation);
    const ids = uniquePositiveIDs(selected.flatMap(conversationTransferMessageIDs));
    const accountIDs = uniquePositiveIDs(selected.flatMap(conversationTransferAccountIDs));
    event.dataTransfer.effectAllowed = "copyMove";
    event.dataTransfer.setData("application/x-rolltop-message-transfer", JSON.stringify({ ids, account_ids: accountIDs }));
    event.dataTransfer.setData("application/x-rolltop-messages", JSON.stringify(ids));
    event.dataTransfer.setData("application/x-rolltop-message", String(ids[0]));
    event.dataTransfer.setData("text/plain", String(ids[0]));
    const dragImage = messageDragPreview(visible, ids);
    if (dragImage) {
      event.dataTransfer.setDragImage(dragImage, 18, 18);
      window.setTimeout(() => dragImage.remove(), 0);
    }
  }

  function selectMessage(event: MouseEvent<HTMLInputElement>, index: number, messageID: number) {
    event.stopPropagation();
    const checked = event.currentTarget.checked;
    const anchorIndex = event.shiftKey && selectionAnchorID.current !== null
      ? visible.findIndex((conversation) => conversation.message.id === selectionAnchorID.current)
      : -1;
    setSelectedIDs((current) => {
      const next = new Set(current);
      if (anchorIndex >= 0) {
        const start = Math.min(anchorIndex, index);
        const end = Math.max(anchorIndex, index);
        for (const conversation of visible.slice(start, end + 1)) {
          if (checked) next.add(conversation.message.id);
          else next.delete(conversation.message.id);
        }
      } else if (checked) {
        next.add(messageID);
      } else {
        next.delete(messageID);
      }
      return next;
    });
    if (!event.shiftKey || anchorIndex < 0) selectionAnchorID.current = messageID;
  }

  function clearSelection() {
    setSelectedIDs(new Set());
    selectionAnchorID.current = null;
  }

  function selectAllOnPage() {
    setSelectedIDs(new Set(visible.map((conversation) => conversation.message.id)));
    selectionAnchorID.current = null;
  }

  async function markSelectedRead(read: boolean) {
    const selected = visible.filter((conversation) => selectedIDs.has(conversation.message.id));
    const messageIDs = uniquePositiveIDs(selected.flatMap(conversationTransferMessageIDs));
    if (messageIDs.length === 0 || selectionBusy) return;
    const previous = selected.map((conversation) => ({ id: conversation.message.id, read: conversation.is_read }));
    onReadStatesChange(selected.map((conversation) => ({ id: conversation.message.id, read })));
    setReadStateBusy(true);
    try {
      await api.bulkRead(csrf, messageIDs, read);
    } catch (err) {
      onReadStatesChange(previous);
      addToast(`${read ? "Mark read" : "Mark unread"} failed: ${messageFromError(err)}`, "error");
    } finally {
      setReadStateBusy(false);
    }
  }

  // Deleting moves the selection into each account's Trash mailbox through the
  // same IMAP-mirrored move APIs as drag and swipe. The mutation is deferred
  // behind an undo toast; the commit tracks per-message progress so a partial
  // failure only restores rows whose messages did not move.
  function deleteSelected() {
    if (selectionBusy) return;
    const selected = visible.filter((conversation) =>
      selectedIDs.has(conversation.message.id) && !pendingSwipeActionIDs.current.has(conversation.message.id));
    if (selected.length === 0) {
      if (selectedIDs.size > 0) addToast("Selected messages are still finishing another action.", "error");
      return;
    }
    const groups = new Map<number, TrashMoveGroup>();
    const skippedTrashNames = new Set<string>();
    let alreadyInTrash = 0;
    for (const conversation of selected) {
      const accountIDs = conversationTransferAccountIDs(conversation);
      if (accountIDs.length !== 1) {
        addToast("Cannot delete a conversation containing messages from multiple accounts.", "error");
        return;
      }
      const target = trashMailboxForAccount(mailboxes, accountIDs[0]);
      if (!target) {
        addToast("Choose a Trash folder for this account before deleting messages.", "error");
        return;
      }
      const messageIDs = conversationTransferMessageIDs(conversation);
      // Skip rows that are certainly already in Trash: any row while viewing
      // that Trash folder, or a single-message row whose message lives there.
      // Multi-message threads elsewhere still move — their representative
      // message may sit in Trash while older messages remain in the Inbox.
      if (target.id === currentMailboxID || (messageIDs.length === 1 && conversation.message.mailbox_id === target.id)) {
        alreadyInTrash++;
        skippedTrashNames.add(target.name);
        continue;
      }
      const group = groups.get(target.id) || { target, messageIDs: [], items: [] };
      group.items.push({ rowID: conversation.message.id, messageIDs });
      group.messageIDs.push(...messageIDs);
      groups.set(target.id, group);
    }
    const skippedLabel = skippedTrashNames.size === 1 ? Array.from(skippedTrashNames)[0] : "Trash";
    if (groups.size === 0) {
      addToast(alreadyInTrash === 1 ? `Message is already in ${skippedLabel}.` : `Messages are already in ${skippedLabel}.`);
      return;
    }
    const entries = Array.from(groups.values());
    for (const entry of entries) entry.messageIDs = uniquePositiveIDs(entry.messageIDs);
    const destLabel = entries.length === 1 ? entries[0].target.name : "Trash";
    const rowIDs = entries.flatMap((entry) => entry.items.map((item) => item.rowID));
    // Dismiss every thread message ID too, so sibling rows of the same thread
    // are hidden and locked during the undo window, matching the swipe path.
    const dismissIDs = uniquePositiveIDs([...rowIDs, ...entries.flatMap((entry) => entry.messageIDs)]);
    const totalMessages = entries.reduce((sum, entry) => sum + entry.messageIDs.length, 0);
    const registered = deferSwipeMutation(
      rowIDs[0],
      `Moved ${messageCountLabel(totalMessages)} to ${destLabel}.`,
      () => {
        removePendingSwipeMoveIDs(dismissIDs);
        restoreDismissed(dismissIDs);
        setSelectedIDs((current) => new Set([...current, ...rowIDs]));
      },
      (keepalive) => commitTrashMove(entries, dismissIDs, destLabel, keepalive)
    );
    if (!registered) return;
    if (alreadyInTrash > 0) addToast(`Skipped ${messageCountLabel(alreadyInTrash)} already in ${skippedLabel}.`);
    setPendingSwipeMoveIDs((current) => new Set([...current, ...dismissIDs]));
    optimisticallyDismiss(dismissIDs);
    clearSelection();
  }

  async function commitTrashMove(entries: TrashMoveGroup[], dismissIDs: number[], destLabel: string, keepalive: boolean) {
    // On a background commit the unsnooze requests must reach the browser
    // before any await, or the unload cancels them; deleting a snoozed message
    // includes dismissing its reminder, so this does not wait for the moves.
    if (snoozedView && keepalive) {
      void Promise.allSettled(entries.flatMap((entry) =>
        entry.items.map((item) => api.unsnoozeMessage(csrf, item.rowID, { keepalive: true }))));
    }
    const movedMessageIDs: number[] = [];
    const movedRowIDs: number[] = [];
    const restoreIDs: number[] = [];
    const reselectRowIDs: number[] = [];
    let queuedMessages = 0;
    let firstError: unknown;
    setTrashOps((count) => count + 1);
    try {
      await Promise.all(entries.map(async (entry) => {
        const { movedIDs, queuedCount, error } = await executeMailboxMove(entry.target, entry.messageIDs, keepalive);
        if (error !== undefined && firstError === undefined) firstError = error;
        queuedMessages += queuedCount;
        movedMessageIDs.push(...movedIDs);
        const movedSet = new Set(movedIDs);
        for (const item of entry.items) {
          if (item.messageIDs.some((id) => movedSet.has(id))) {
            movedRowIDs.push(item.rowID);
          } else {
            restoreIDs.push(item.rowID, ...item.messageIDs);
            reselectRowIDs.push(item.rowID);
          }
        }
      }));
    } finally {
      setTrashOps((count) => Math.max(0, count - 1));
    }
    if (snoozedView && !keepalive && movedRowIDs.length > 0) {
      void Promise.allSettled(movedRowIDs.map((rowID) => api.unsnoozeMessage(csrf, rowID)));
    }
    if (movedMessageIDs.length > 0) onMessagesMoved(movedMessageIDs);
    removePendingSwipeMoveIDs(dismissIDs);
    if (restoreIDs.length > 0) {
      restoreDismissed(uniquePositiveIDs(restoreIDs));
      setSelectedIDs((current) => new Set([...current, ...reselectRowIDs]));
    }
    if (queuedMessages > 0) addToast(`Move to ${destLabel} started for ${messageCountLabel(queuedMessages)}.`);
    if (firstError !== undefined) addToast(`Delete failed: ${messageFromError(firstError)}`, "error");
  }

  // executeMailboxMove pushes messageIDs into the target mailbox and reports
  // which messages moved (or were queued as a background run) so callers can
  // reconcile rows without guessing. Shared by swipe moves and bulk delete.
  async function executeMailboxMove(target: Mailbox, messageIDs: number[], keepalive: boolean): Promise<{ movedIDs: number[]; queuedCount: number; error?: unknown }> {
    if (keepalive || messageIDs.length > inlineMoveMessageLimit) {
      // Chunk here (within the backend's batch cap) so each chunk's outcome is
      // tracked independently: one failed chunk must not discard the moved IDs
      // of chunks the backend already accepted.
      const chunks: number[][] = [];
      for (let start = 0; start < messageIDs.length; start += bulkMessageIDLimit) {
        chunks.push(messageIDs.slice(start, start + bulkMessageIDLimit));
      }
      const dispatched = keepalive ? chunks.slice(0, keepaliveMoveChunkBudget) : chunks;
      const results = await Promise.allSettled(dispatched.map((chunk) =>
        api.bulkMoveMessages(csrf, chunk, target.id, keepalive ? { keepalive: true } : undefined)));
      const movedIDs: number[] = [];
      let queuedCount = 0;
      let error: unknown;
      results.forEach((result, index) => {
        if (result.status === "fulfilled") {
          movedIDs.push(...dispatched[index]);
          if (result.value.queued) queuedCount += dispatched[index].length;
        } else if (error === undefined) {
          error = result.reason;
        }
      });
      return { movedIDs, queuedCount, error };
    }
    const movedIDs: number[] = [];
    let error: unknown;
    for (const messageID of messageIDs) {
      try {
        await api.moveMessage(csrf, messageID, target.id);
        movedIDs.push(messageID);
      } catch (err) {
        if (error === undefined) error = err;
      }
    }
    return { movedIDs, queuedCount: 0, error };
  }

  function optimisticallyDismiss(ids: number[]) {
    setDismissedIDs((current) => new Set([...current, ...ids]));
    setSelectedIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
  }

  function restoreDismissed(ids: number[]) {
    setDismissedIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
  }

  function settleSwipeRow(messageID: number) {
    setSwipeState((current) => current?.id === messageID
      ? { ...current, deltaX: 0, phase: "settling", committed: false }
      : current);
    clearSwipeStateAfter(messageID, messageSwipeSettleMS);
  }

  function beginSwipeDismiss(messageID: number, ids: number[], direction: "start" | "end") {
    const existingTimer = swipeDismissTimers.current.get(messageID);
    if (existingTimer !== undefined) window.clearTimeout(existingTimer);
    const reduceMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    if (reduceMotion) {
      optimisticallyDismiss(ids);
      setSwipeState((current) => current?.id === messageID ? null : current);
      return;
    }
    const row = rowRefs.current.get(messageID);
    const bounds = row?.getBoundingClientRect();
    const distance = Math.max(bounds?.width || 0, window.innerWidth) + 24;
    const rowHeight = Math.ceil(bounds?.height || row?.offsetHeight || 72);
    const exitDelta = direction === "start" ? distance : -distance;
    setSelectedIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
    setSwipeState((current) => current?.id === messageID
      ? {
          ...current,
          deltaX: exitDelta,
          visualDeltaX: exitDelta,
          direction,
          phase: "exiting",
          committed: true,
          rowHeight
        }
      : current);
    const timer = window.setTimeout(() => {
      swipeDismissTimers.current.delete(messageID);
      optimisticallyDismiss(ids);
      setSwipeState((current) => current?.id === messageID ? null : current);
    }, messageSwipeExitMS);
    swipeDismissTimers.current.set(messageID, timer);
  }

  function cancelSwipeDismiss(messageID: number) {
    const timer = swipeDismissTimers.current.get(messageID);
    if (timer !== undefined) {
      window.clearTimeout(timer);
      swipeDismissTimers.current.delete(messageID);
      settleSwipeRow(messageID);
    }
  }

  function removePendingSwipeMoveIDs(ids: number[]) {
    setPendingSwipeMoveIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
  }

  function removePendingSwipeSnoozeIDs(ids: number[]) {
    setPendingSwipeSnoozeIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
  }

  function removePendingSwipeReadState(id: number) {
    setPendingSwipeReadStates((current) => {
      if (!current.has(id)) return current;
      const next = new Map(current);
      next.delete(id);
      return next;
    });
  }

  function deferSwipeMutation(
    conversationID: number,
    message: string,
    onUndo: () => void,
    onCommit: (keepalive: boolean) => Promise<void>
  ): boolean {
    if (pendingSwipeActionIDs.current.has(conversationID)) return false;
    pendingSwipeActionIDs.current.add(conversationID);
    let settled = false;
    addToast(message, "success", {
      onUndo: () => {
        if (settled) return;
        settled = true;
        pendingSwipeActionIDs.current.delete(conversationID);
        onUndo();
      },
      onCommit: (reason) => {
        if (settled) return;
        settled = true;
        void onCommit(reason === "background")
          .catch((err) => addToast(`Swipe action failed: ${messageFromError(err)}`, "error"))
          .finally(() => pendingSwipeActionIDs.current.delete(conversationID));
      }
    });
    return true;
  }

  async function snoozeConversations(items: Conversation[], until: Date) {
    const ids = uniquePositiveIDs(items.map((conversation) => conversation.message.id));
    if (ids.length === 0) return;
    if (selectionBusy) {
      addToast("Another action is still running. Try snoozing again in a moment.", "error");
      return;
    }
    if (!snoozedView) optimisticallyDismiss(ids);
    clearSelection();
    setSnoozeBusy(true);
    try {
      const results = await Promise.allSettled(ids.map((id) => api.snoozeMessage(csrf, id, until)));
      const failed = ids.filter((_, index) => results[index].status === "rejected");
      if (!snoozedView && failed.length > 0) restoreDismissed(failed);
      const succeeded = ids.length - failed.length;
      if (succeeded > 0) addToast(`${succeeded === 1 ? "Message" : `${succeeded.toLocaleString()} messages`} snoozed until ${displaySnoozeUntil(until, datePrefs)}.`);
      if (failed.length > 0) {
        const first = results.find((result) => result.status === "rejected");
        const reason = first?.status === "rejected" ? messageFromError(first.reason) : "Request failed";
        addToast(`${failed.length.toLocaleString()} snooze ${failed.length === 1 ? "request" : "requests"} failed: ${reason}`, "error");
        throw first?.status === "rejected" ? first.reason : new Error(reason);
      }
    } finally {
      setSnoozeBusy(false);
    }
  }

  async function unsnoozeConversations(items: Conversation[]) {
    const ids = uniquePositiveIDs(items.map((conversation) => conversation.message.id));
    if (ids.length === 0) return;
    if (selectionBusy) {
      addToast("Another action is still running. Try unsnoozing again in a moment.", "error");
      return;
    }
    optimisticallyDismiss(ids);
    clearSelection();
    setSnoozeBusy(true);
    try {
      const results = await Promise.allSettled(ids.map((id) => api.unsnoozeMessage(csrf, id)));
      const failed = ids.filter((_, index) => results[index].status === "rejected");
      if (failed.length > 0) restoreDismissed(failed);
      const succeeded = ids.length - failed.length;
      if (succeeded > 0) addToast(`${succeeded === 1 ? "Message" : `${succeeded.toLocaleString()} messages`} returned to mail.`);
      if (failed.length > 0) {
        const first = results.find((result) => result.status === "rejected");
        const reason = first?.status === "rejected" ? messageFromError(first.reason) : "Request failed";
        addToast(`${failed.length.toLocaleString()} unsnooze ${failed.length === 1 ? "request" : "requests"} failed: ${reason}`, "error");
      }
    } finally {
      setSnoozeBusy(false);
    }
  }

  function markConversationRead(conversation: Conversation, read: boolean) {
    const ids = conversationTransferMessageIDs(conversation);
    const previous = conversation.is_read;
    if (previous === read) {
      addToast(`Message is already ${read ? "read" : "unread"}.`);
      return;
    }
    const rowID = conversation.message.id;
    const registered = deferSwipeMutation(
      rowID,
      `Message marked ${read ? "read" : "unread"}.`,
      () => {
        removePendingSwipeReadState(rowID);
        onReadStatesChange([{ id: rowID, read: previous }]);
      },
      async (keepalive) => {
        try {
          await api.bulkRead(csrf, ids, read, { keepalive });
          onReadStatesChange([{ id: rowID, read }]);
        } catch (err) {
          onReadStatesChange([{ id: rowID, read: previous }]);
          addToast(`${read ? "Mark read" : "Mark unread"} failed: ${messageFromError(err)}`, "error");
        } finally {
          removePendingSwipeReadState(rowID);
        }
      }
    );
    if (registered) {
      setPendingSwipeReadStates((current) => new Map(current).set(rowID, read));
      onReadStatesChange([{ id: rowID, read }]);
      clearSelection();
    }
  }

  function snoozeConversationBySwipe(conversation: Conversation, until: Date, direction: "start" | "end"): boolean {
    const ids = [conversation.message.id];
    const rowID = conversation.message.id;
    const registered = deferSwipeMutation(
      rowID,
      `Message snoozed until ${displaySnoozeUntil(until, datePrefs)}.`,
      () => {
        cancelSwipeDismiss(rowID);
        if (!snoozedView) {
          removePendingSwipeSnoozeIDs(ids);
          restoreDismissed(ids);
        }
      },
      async (keepalive) => {
        try {
          await api.snoozeMessage(csrf, rowID, until, { keepalive });
          if (!snoozedView) {
            onMessagesMoved(ids);
            removePendingSwipeSnoozeIDs(ids);
          }
        } catch (err) {
          cancelSwipeDismiss(rowID);
          if (!snoozedView) {
            removePendingSwipeSnoozeIDs(ids);
            restoreDismissed(ids);
          }
          addToast(`Snooze failed: ${messageFromError(err)}`, "error");
        }
      }
    );
    if (!registered) return false;
    if (!snoozedView) {
      setPendingSwipeSnoozeIDs((current) => new Set([...current, ...ids]));
      beginSwipeDismiss(rowID, ids, direction);
    }
    clearSelection();
    return !snoozedView;
  }

  function moveConversationBySwipe(conversation: Conversation, action: "trash" | "archive", direction: "start" | "end"): boolean {
    const accountIDs = conversationTransferAccountIDs(conversation);
    if (accountIDs.length !== 1) {
      addToast(`Cannot ${action} a conversation containing messages from multiple accounts.`, "error");
      return false;
    }
    const accountID = accountIDs[0];
    const target = action === "trash"
      ? trashMailboxForAccount(mailboxes, accountID)
      : (() => {
          const preference = effectiveSwipePreferences.archive_mailboxes.find((item) => item.account_id === accountID);
          return preference
            ? mailboxes.find((mailbox) => mailbox.id === preference.mailbox_id && mailbox.account_id === accountID && mailbox.role === "")
            : undefined;
        })();
    if (!target) {
      addToast(
        action === "trash"
          ? "Choose a Trash folder for this account before using the trash swipe action."
          : "Choose an Archive folder for this account in swipe settings.",
        "error"
      );
      return false;
    }
    if (conversation.message.mailbox_id === target.id) {
      addToast(`This conversation is already in ${target.name}.`);
      return false;
    }
    const messageIDs = conversationTransferMessageIDs(conversation);
    const dismissedIDs = messageIDs;
    const registered = deferSwipeMutation(
      conversation.message.id,
      `Moved ${messageCountLabel(messageIDs.length)} to ${target.name}.`,
      () => {
        cancelSwipeDismiss(conversation.message.id);
        removePendingSwipeMoveIDs(dismissedIDs);
        restoreDismissed(dismissedIDs);
      },
      async (keepalive) => {
        const { movedIDs, error } = await executeMailboxMove(target, messageIDs, keepalive);
        removePendingSwipeMoveIDs(dismissedIDs);
        if (error === undefined) {
          onMessagesMoved(messageIDs);
          return;
        }
        if (movedIDs.length > 0) onMessagesMoved(movedIDs);
        else {
          cancelSwipeDismiss(conversation.message.id);
          restoreDismissed(dismissedIDs);
        }
        const partial = movedIDs.length > 0 ? `${movedIDs.length.toLocaleString()} moved, but the remaining action failed` : `${action === "trash" ? "Move to trash" : "Archive"} failed`;
        addToast(`${partial}: ${messageFromError(error)}`, "error");
      }
    );
    if (!registered) return false;
    setPendingSwipeMoveIDs((current) => new Set([...current, ...dismissedIDs]));
    beginSwipeDismiss(conversation.message.id, dismissedIDs, direction);
    return true;
  }

  async function executeSwipeAction(
    conversation: Conversation,
    action: SwipeAction,
    snoozePreset: SwipePreferences["left_snooze_preset"],
    direction: "start" | "end"
  ): Promise<boolean> {
    if (selectionBusy || pendingSwipeActionIDs.current.has(conversation.message.id)) return false;
    setSwipeActionBusy(true);
    try {
      switch (action) {
      case "mark_read":
        await markConversationRead(conversation, true);
        return false;
      case "mark_unread":
        await markConversationRead(conversation, false);
        return false;
      case "snooze":
        return snoozeConversationBySwipe(conversation, swipeSnoozeUntil(snoozePreset), direction);
      case "trash":
      case "archive":
        return moveConversationBySwipe(conversation, action, direction);
      }
    } finally {
      setSwipeActionBusy(false);
    }
    return false;
  }

  function startRowSwipe(event: TouchEvent<HTMLDivElement>, conversation: Conversation) {
    if (selectionBusy || swipeState || pendingSwipeActionIDs.current.has(conversation.message.id) || !nativeTouchDrag || event.touches.length !== 1 || (event.target as HTMLElement).closest("button,input,label")) return;
    const touch = event.touches[0];
    swipeSession.current = { id: conversation.message.id, startX: touch.clientX, startY: touch.clientY, lastX: touch.clientX, lastY: touch.clientY, active: false, blocked: false };
  }

  function moveRowSwipe(event: TouchEvent<HTMLDivElement>) {
    const session = swipeSession.current;
    if (!session || event.touches.length !== 1) return;
    if (document.documentElement.classList.contains("rolltop-touch-message-dragging")) {
      swipeSession.current = null;
      setSwipeState(null);
      return;
    }
    const touch = event.touches[0];
    session.lastX = touch.clientX;
    session.lastY = touch.clientY;
    const deltaX = touch.clientX - session.startX;
    const deltaY = touch.clientY - session.startY;
    if (!session.active && !session.blocked) {
      if (Math.abs(deltaY) > 10 && Math.abs(deltaY) >= Math.abs(deltaX)) session.blocked = true;
      else if (Math.abs(deltaX) > 12 && Math.abs(deltaX) > Math.abs(deltaY) * 1.15) session.active = true;
    }
    if (!session.active) return;
    event.preventDefault();
    const clampedDeltaX = Math.max(-messageSwipeMaxDistance, Math.min(messageSwipeMaxDistance, deltaX));
    setSwipeState({
      id: session.id,
      deltaX: clampedDeltaX,
      visualDeltaX: clampedDeltaX,
      direction: clampedDeltaX > 0 ? "start" : "end",
      phase: "tracking",
      committed: false
    });
  }

  function clearSwipeStateAfter(messageID: number, delay: number) {
    if (swipeCompletionTimer.current !== null) window.clearTimeout(swipeCompletionTimer.current);
    swipeCompletionTimer.current = window.setTimeout(() => {
      swipeCompletionTimer.current = null;
      setSwipeState((current) => current?.id === messageID ? null : current);
    }, delay);
  }

  function finishRowSwipe(conversation: Conversation) {
    const session = swipeSession.current;
    swipeSession.current = null;
    if (document.documentElement.classList.contains("rolltop-touch-message-dragging")) {
      setSwipeState(null);
      return;
    }
    if (!session || !session.active) {
      setSwipeState(null);
      return;
    }
    const deltaX = session.lastX - session.startX;
    suppressRowClickUntil.current = Date.now() + 450;
    const direction = deltaX > 0 ? "start" : "end";
    const clampedDeltaX = Math.max(-messageSwipeMaxDistance, Math.min(messageSwipeMaxDistance, deltaX));
    if (Math.abs(deltaX) < messageSwipeCommitDistance) {
      setSwipeState({
        id: session.id,
        deltaX: 0,
        visualDeltaX: clampedDeltaX,
        direction,
        phase: "settling",
        committed: false
      });
      clearSwipeStateAfter(session.id, messageSwipeSettleMS);
      return;
    }
    const committedDeltaX = direction === "start" ? messageSwipeMaxDistance : -messageSwipeMaxDistance;
    const action = direction === "start" ? effectiveSwipePreferences.right_action : effectiveSwipePreferences.left_action;
    const snoozePreset = direction === "start" ? effectiveSwipePreferences.right_snooze_preset : effectiveSwipePreferences.left_snooze_preset;
    setSwipeState({
      id: session.id,
      deltaX: committedDeltaX,
      visualDeltaX: committedDeltaX,
      direction,
      phase: "committing",
      committed: true
    });
    if (swipeCompletionTimer.current !== null) window.clearTimeout(swipeCompletionTimer.current);
    swipeCompletionTimer.current = window.setTimeout(() => {
      swipeCompletionTimer.current = null;
      void executeSwipeAction(conversation, action, snoozePreset, direction)
        .then((dismissStarted) => {
          if (!dismissStarted) settleSwipeRow(session.id);
        })
        .catch(() => settleSwipeRow(session.id));
    }, messageSwipeCommitHoldMS);
  }

  async function toggleStar(event: MouseEvent<HTMLButtonElement>, conversation: Conversation) {
    event.preventDefault();
    event.stopPropagation();
    const msg = conversation.message;
    const targetID = conversation.starred_message_id || msg.id;
    const next = !msg.is_starred;
    onStarredChange(msg.id, targetID, next);
    try {
      await api.setStarred(csrf, targetID, next);
    } catch (err) {
      onStarredChange(msg.id, targetID, msg.is_starred);
      addToast(`Star update failed: ${messageFromError(err)}`, "error");
    }
  }

  function openRow(event: MouseEvent<HTMLDivElement>, href: string) {
    if (Date.now() < suppressRowClickUntil.current) return;
    if ((event.target as HTMLElement).closest("button,input,label")) return;
    navigate(href);
  }

  function openRowWithKeyboard(event: KeyboardEvent<HTMLDivElement>, href: string) {
    if (event.currentTarget !== event.target) return;
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      navigate(href);
    }
  }

  if (visible.length === 0) {
    return emptyState ?? <div className="panel muted">No messages here.</div>;
  }
  const arrivalActive = visible.some((conversation) => highlightMessageIDs?.has(conversation.message.id));
  const selectedConversations = visible.filter((conversation) => selectedIDs.has(conversation.message.id));
  const allOnPageSelected = selectedConversations.length === visible.length;
  const canMarkRead = selectedConversations.some((conversation) => !conversation.is_read);
  const canMarkUnread = selectedConversations.some((conversation) => conversation.is_read);
  return (
    <div className={`message-table ${arrivalActive ? "mail-arrival-shift" : ""}`}>
      {selectedConversations.length > 0 ? (
        <div className="selection-action-bar" role="toolbar" aria-label="Selected message actions" aria-busy={selectionBusy}>
          <div className="selection-action-summary">
            <button className="selection-clear" type="button" onClick={clearSelection} title="Clear selection" aria-label="Clear selection">
              <Icon name="close" />
            </button>
            <span className="selection-count" aria-live="polite">
              <strong>{selectedConversations.length.toLocaleString()}</strong>
              <span>selected</span>
            </span>
            {allOnPageSelected ? (
              <span className="selection-page-status">All {visible.length.toLocaleString()} on this page</span>
            ) : (
              <button
                className="selection-page-button"
                type="button"
                onClick={selectAllOnPage}
                disabled={selectionBusy}
                title={`Select all ${visible.length.toLocaleString()} messages on this page`}
                aria-label={`Select all ${visible.length.toLocaleString()} messages on this page`}
              >
                <Icon name="select_all" />
                <span>Select all {visible.length.toLocaleString()}</span>
              </button>
            )}
          </div>
          <div className="selection-actions">
            <button type="button" disabled={selectionBusy || !canMarkRead} onClick={() => void markSelectedRead(true)} title="Mark selected messages read">
              <Icon name="mail_open" />
              <span>Mark read</span>
            </button>
            <button type="button" disabled={selectionBusy || !canMarkUnread} onClick={() => void markSelectedRead(false)} title="Mark selected messages unread">
              <Icon name="mail" />
              <span>Mark unread</span>
            </button>
      {snoozedView ? (
        <button type="button" disabled={selectionBusy} onClick={() => void unsnoozeConversations(selectedConversations)} title="Unsnooze selected messages">
          <Icon name="clock" />
          <span>Unsnooze</span>
        </button>
      ) : (
        <SnoozeControl datePrefs={datePrefs} disabled={selectionBusy} onSnooze={(until) => snoozeConversations(selectedConversations, until)} />
      )}
            <button type="button" className="selection-delete" disabled={selectionBusy} onClick={() => void deleteSelected()} title="Move selected messages to Trash">
              <Icon name="delete" />
              <span>Delete</span>
            </button>
          </div>
        </div>
      ) : null}
      {visible.map((conversation, index) => {
        const msg = conversation.message;
        const matchTerms = conversation.match_terms || [];
        const href = openAsDraft ? `/compose?draft=${msg.id}` : messageURL(msg.id, searchQuery, matchTerms, returnURL, searchQuery ? msg.id : 0);
        const attachmentNames = conversation.attachment_names || [];
        const attachmentMatches = conversation.attachment_matches || [];
        const previewText = messageSecurityPreviewText(messageSecurityPlugins, conversation.snippet, msg);
        const securitySnippetClass = messageSecuritySnippetClassName(messageSecurityPlugins, msg);
        const securityIndicators = messageSecurityIndicators(messageSecurityPlugins, { location: "message-list", message: msg, state: msg });
        const annotationNodes = messageAnnotationNodes(messageSecurityPlugins, msg);
        const selected = selectedIDs.has(msg.id);
        const touchMessageIDs = selected && selectedDragMessageIDs.length > 0 ? selectedDragMessageIDs : conversationTransferMessageIDs(conversation);
        const touchAccountIDs = selected && selectedDragAccountIDs.length > 0 ? selectedDragAccountIDs : conversationTransferAccountIDs(conversation);
        const movingOut = hiddenMessageIDs.has(msg.id);
        const activeSwipe = swipeState?.id === msg.id ? swipeState : null;
        const swipeDelta = activeSwipe?.deltaX || 0;
        const swipeReady = Boolean(activeSwipe?.committed || (activeSwipe && Math.abs(activeSwipe.visualDeltaX) >= messageSwipeCommitDistance));
        const swipeStyle = activeSwipe ? messageSwipeAffordanceStyle(activeSwipe) : undefined;
        const participantText = showRecipients
          ? `To: ${conversation.recipient_participants || msg.to_addr || conversation.participants || "undisclosed recipients"}`
          : (conversation.participants || msg.from_addr || "Unknown sender");
        return (
      <div
        className={`message-swipe-shell ${activeSwipe ? `revealing-${activeSwipe.direction} swipe-phase-${activeSwipe.phase}` : ""} ${swipeReady ? "swipe-action-ready" : ""}`}
        key={msg.id}
        style={swipeStyle}
      >
      <div className="message-swipe-actions" aria-hidden="true">
        <span className={`message-swipe-action message-swipe-action-start swipe-action-${rightSwipePresentation.className}`}>
          <span className="message-swipe-action-content">
            <span className="message-swipe-action-icon"><Icon name={rightSwipePresentation.icon} /></span>
            <small>{rightSwipePresentation.label}</small>
          </span>
        </span>
        <span className={`message-swipe-action message-swipe-action-end swipe-action-${leftSwipePresentation.className}`}>
          <span className="message-swipe-action-content">
            <span className="message-swipe-action-icon"><Icon name={leftSwipePresentation.icon} /></span>
            <small>{leftSwipePresentation.label}</small>
          </span>
        </span>
      </div>
      <div
            className={`message-row ${conversation.is_read ? "read" : "unread"} ${selected ? "selected" : ""} ${keyboardIndex === index ? "keyboard-focused" : ""} ${movingOut ? "moving-out" : ""} ${highlightMessageIDs?.has(msg.id) ? "new-delivery" : ""}`}
      style={activeSwipe ? { transform: `translateX(${swipeDelta}px)` } : undefined}
            draggable
            ref={(node) => {
              if (node) rowRefs.current.set(msg.id, node);
              else rowRefs.current.delete(msg.id);
            }}
            data-rolltop-message-drag="true"
            data-rolltop-list-index={index}
            data-rolltop-touch-drag={nativeTouchDrag ? "true" : undefined}
            data-rolltop-touch-message-ids={nativeTouchDrag ? touchMessageIDs.join(",") : undefined}
            data-rolltop-touch-account-ids={nativeTouchDrag ? touchAccountIDs.join(",") : undefined}
            role="link"
            tabIndex={0}
            onClick={(event) => openRow(event, href)}
            onFocus={() => {
              keyboardIndexRef.current = index;
              setKeyboardIndex(index);
            }}
            onKeyDown={(event) => openRowWithKeyboard(event, href)}
            onDragStart={(event) => startMessageDrag(event, conversation)}
      onTouchStart={(event) => startRowSwipe(event, conversation)}
      onTouchMove={moveRowSwipe}
      onTouchEnd={() => finishRowSwipe(conversation)}
      onTouchCancel={() => { swipeSession.current = null; setSwipeState(null); }}
          >
            <label
              className={`message-select ${selected && selectedIDs.size > 1 ? "group-drag-source" : ""}`}
              draggable={selected}
              onClick={(event) => event.stopPropagation()}
              title={selected && selectedIDs.size > 1 ? `Drag ${selectedIDs.size.toLocaleString()} selected messages or clear selection` : "Select message"}
            >
              <input
                type="checkbox"
                checked={selected}
                disabled={pendingSwipeActionIDs.current.has(msg.id)}
                aria-label={`Select ${msg.subject || "message"}`}
                onClick={(event) => selectMessage(event, index, msg.id)}
                onChange={() => undefined}
              />
            </label>
            <button
              className={`star-action ${msg.is_starred ? "starred" : ""}`}
              type="button"
              aria-pressed={msg.is_starred}
              title={msg.is_starred ? "Unstar" : "Star"}
              onClick={(event) => void toggleStar(event, conversation)}
            >
              <Star className="icon" weight={msg.is_starred ? "fill" : "regular"} />
            </button>
            <span className="sender">
              <span className="sender-name">
                <HighlightedText text={participantText} query={searchQuery} terms={matchTerms} />
              </span>
              {conversation.count > 1 ? <span className="thread-count">({conversation.count})</span> : null}
            </span>
            <span className="subject">
              <strong>
                <HighlightedText text={msg.subject || "(no subject)"} query={searchQuery} terms={matchTerms} />
              </strong>
              {securityIndicators}
              {annotationNodes}
              <span className={`snippet ${securitySnippetClass}`}>
                <HighlightedText text={previewText} query={securitySnippetClass ? "" : searchQuery} terms={securitySnippetClass ? [] : matchTerms} />
              </span>
              {attachmentNames.length > 0 ? (
                <span className={`attachment-preview ${attachmentMatches.length > 0 || conversation.attachment_content_matched ? "matched" : ""}`}>
                  <Icon name="attach_file" />
                  <HighlightedText
                    text={attachmentMatches.length > 0 ? attachmentMatches.join(", ") : attachmentNames.join(", ")}
                    query={searchQuery}
                    terms={matchTerms}
                  />
                </span>
              ) : conversation.has_attachments ? <Icon name="attach_file" /> : null}
            </span>
      <span className={`date ${snoozedView ? "snoozed-date" : ""}`}>
        {snoozedView ? (
          <button className="snooze-row-action" type="button" disabled={selectionBusy} onClick={() => void unsnoozeConversations([conversation])} title="Unsnooze" aria-label="Unsnooze">
            <Icon name="clock" />
          </button>
        ) : null}
        <span>{displayTime(snoozedView && conversation.snoozed_until ? conversation.snoozed_until : msg.date, datePrefs)}</span>
      </span>
      </div>
          </div>
        );
      })}
    </div>
  );
}
