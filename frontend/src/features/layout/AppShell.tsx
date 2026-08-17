// File overview: Authenticated application chrome: top bar, search entry, folder sidebar, mobile
// drawer, drag-to-folder handling, sync status, and the mobile compose affordance.

import { useMemo, useState, useEffect, useRef } from "react";
import type { DragEvent, FormEvent, MouseEvent, ReactNode } from "react";
import { api } from "../../api";
import type { AppShellProps, LocationState, MessageTransferAction, MoveTarget, SecurityUnlockState } from "../../appTypes";
import type { Bootstrap, Mailbox, SyncRun, User } from "../../types";
import { Icon, LogoMark } from "../../components/Icon";
import { androidNativeAvailable, shouldAdvertiseAndroidApp } from "../../lib/androidNative";
import { folderTree, folderTreeUnreadCount, nodeContainsMailbox, type FolderNode } from "../../lib/folders";
import { shouldIgnoreMailShortcut } from "../../lib/keyboard";
import { mailRoute, mailURL, searchRoute, searchURL, currentLocation } from "../../lib/routes";
import { loadCollapsedAccounts, saveCollapsedAccounts } from "../../lib/sidebarLocal";
import { createPluginSet } from "../../plugins/registry";
import { SearchAutocomplete, useSearchAutocomplete } from "./SearchAutocomplete";

/**
 * AppShell renders everything that survives route changes after login: topbar,
 * folder navigation, sync widget, account warnings, mobile drawer state, and the
 * floating compose action. Children supply only the current content view.
 */
export function AppShell({
  user,
  csrf,
  mailboxes,
  latestSyncRun,
  activeSyncRuns,
  syncRunning,
  serverStartedAt,
  serverUptimeSeconds,
  buildVersion,
  buildDate,
  buildLabel,
  buildCommit,
  accountNeedsPassword,
  accountNotice,
  enabledPlugins,
  location,
  navigate,
  onMoveMessages,
  openCompose,
  refreshChrome,
  notificationsEnabled,
  toggleNotifications,
  securityUnlockAvailable,
  securityUnlock,
  openSecurityUnlock,
  lockSecurity,
  logout,
  children
}: AppShellProps) {
  const [mobileSidebarOpen, setMobileSidebarOpen] = useState(false);
  const [messageDragActive, setMessageDragActive] = useState(false);
  const [touchDragPreview, setTouchDragPreview] = useState<TouchDragPreview | null>(null);
  const [touchDropID, setTouchDropID] = useState<number | null>(null);
  const [touchRevealedAccounts, setTouchRevealedAccounts] = useState<string[]>([]);
  const appRef = useRef<HTMLDivElement>(null);
  const dragOpenedSidebar = useRef(false);
  const nativeDragInProgress = useRef(false);
  const nativeDragActivationTimer = useRef<number | null>(null);
  const mobileSidebarOpenRef = useRef(mobileSidebarOpen);
  const onMoveMessagesRef = useRef(onMoveMessages);

  mobileSidebarOpenRef.current = mobileSidebarOpen;
  onMoveMessagesRef.current = onMoveMessages;

  useEffect(() => {
    return () => {
      if (nativeDragActivationTimer.current !== null) window.clearTimeout(nativeDragActivationTimer.current);
    };
  }, []);

  // WebView's native touch drag is canceled by drawer hit-test changes, so the
  // Android shell uses a long-press gesture while mouse/desktop drag stays native.
  useEffect(() => {
    const root = appRef.current;
    if (!root || !androidNativeAvailable()) return;
    const appRoot = root;

    let session: AndroidTouchDragSession | null = null;
    let sessionEventTarget: HTMLElement | null = null;
    let sessionListenersAttached = false;
    let suspendedNativeDragElements: HTMLElement[] = [];
    let suppressCompatibilityClick = false;
    let compatibilityClickX = 0;
    let compatibilityClickY = 0;
    let compatibilityClickTimer: number | null = null;
    let touchClassRemovalTimer: number | null = null;

    function clearCompatibilityClickTimer() {
      if (compatibilityClickTimer === null) return;
      window.clearTimeout(compatibilityClickTimer);
      compatibilityClickTimer = null;
    }

    function expireCompatibilityClickGuard() {
      clearCompatibilityClickTimer();
      compatibilityClickTimer = window.setTimeout(() => {
        compatibilityClickTimer = null;
        suppressCompatibilityClick = false;
      }, 400);
    }

    function clearTouchClassRemovalTimer() {
      if (touchClassRemovalTimer === null) return;
      window.clearTimeout(touchClassRemovalTimer);
      touchClassRemovalTimer = null;
    }

    function removeTouchDragClassAfterEvent() {
      clearTouchClassRemovalTimer();
      touchClassRemovalTimer = window.setTimeout(() => {
        touchClassRemovalTimer = null;
        document.documentElement.classList.remove("rolltop-touch-message-dragging");
      }, 0);
    }

    function clearHoldTimer() {
      if (!session || session.holdTimer === null) return;
      window.clearTimeout(session.holdTimer);
      session.holdTimer = null;
    }

    function suspendNativeDrag(source: HTMLElement) {
      restoreNativeDrag();
      const candidates = [source, ...Array.from(source.querySelectorAll<HTMLElement>("[draggable='true']"))];
      suspendedNativeDragElements = candidates.filter((element, index) => element.draggable && candidates.indexOf(element) === index);
      suspendedNativeDragElements.forEach((element) => { element.draggable = false; });
    }

    function restoreNativeDrag() {
      suspendedNativeDragElements.forEach((element) => { element.draggable = true; });
      suspendedNativeDragElements = [];
    }

    function closeAutoOpenedSidebar() {
      if (!dragOpenedSidebar.current) return;
      dragOpenedSidebar.current = false;
      mobileSidebarOpenRef.current = false;
      setMobileSidebarOpen(false);
    }

    function resetTouchDrag() {
      const wasActive = session?.active === true;
      if (session) {
        compatibilityClickX = session.lastX;
        compatibilityClickY = session.lastY;
      }
      clearHoldTimer();
      session = null;
      detachSessionListeners();
      restoreNativeDrag();
      if (!wasActive) return;
      removeTouchDragClassAfterEvent();
      setMessageDragActive(false);
      setTouchDragPreview(null);
      setTouchDropID(null);
      setTouchRevealedAccounts((current) => current.length > 0 ? [] : current);
      closeAutoOpenedSidebar();
      expireCompatibilityClickGuard();
    }

    function activateTouchDrag() {
      if (!session || session.active) return;
      session.holdTimer = null;
      session.active = true;
      session.startX = session.lastX;
      session.startY = session.lastY;
      suppressCompatibilityClick = true;
      compatibilityClickX = session.lastX;
      compatibilityClickY = session.lastY;
      clearCompatibilityClickTimer();
      clearTouchClassRemovalTimer();
      document.documentElement.classList.add("rolltop-touch-message-dragging");
      setMessageDragActive(true);
      setTouchDragPreview(touchPreviewAt(session.lastX, session.lastY, session.messageIDs.length));
      if (!mobileSidebarOpenRef.current) {
        dragOpenedSidebar.current = true;
        mobileSidebarOpenRef.current = true;
        setMobileSidebarOpen(true);
      }
    }

    function start(event: TouchEvent) {
      if (!session) {
        suppressCompatibilityClick = false;
        clearCompatibilityClickTimer();
      }
      if (event.touches.length !== 1) {
        if (session) resetTouchDrag();
        return;
      }
      if (session) return;
      const target = event.target;
      const source = target instanceof Element ? target.closest<HTMLElement>("[data-rolltop-touch-drag='true']") : null;
      if (!source || !appRoot.contains(source)) return;
      const messageIDs = positiveIDs(source.dataset.rolltopTouchMessageIds);
      if (messageIDs.length === 0) return;
      const touch = event.touches[0];
      suspendNativeDrag(source);
      session = {
        identifier: touch.identifier,
        startX: touch.clientX,
        startY: touch.clientY,
        lastX: touch.clientX,
        lastY: touch.clientY,
        messageIDs,
        accountIDs: positiveIDs(source.dataset.rolltopTouchAccountIds),
        active: false,
        movedAfterActivation: false,
        holdTimer: window.setTimeout(activateTouchDrag, androidTouchDragHoldMS)
      };
      attachSessionListeners(source);
    }

    function move(event: TouchEvent) {
      if (!session) return;
      if (event.touches.length !== 1) {
        resetTouchDrag();
        return;
      }
      const touch = touchWithIdentifier(event.touches, session.identifier);
      if (!touch) return;
      session.lastX = touch.clientX;
      session.lastY = touch.clientY;
      if (!session.active) {
        if (Math.hypot(touch.clientX - session.startX, touch.clientY - session.startY) > androidTouchDragSlop) {
          resetTouchDrag();
        }
        return;
      }
      if (event.cancelable) event.preventDefault();
      setTouchDragPreview(touchPreviewAt(touch.clientX, touch.clientY, session.messageIDs.length));
      if (Math.hypot(touch.clientX - session.startX, touch.clientY - session.startY) > androidTouchDragSlop) {
        session.movedAfterActivation = true;
      }
      setTouchDropID(session.movedAfterActivation ? touchDropTargetAt(touch.clientX, touch.clientY)?.id ?? null : null);
      // Touch drags cannot fire dragenter, so dwelling on a collapsed account
      // header is what opens it up as a drop target.
      const accountKey = session.movedAfterActivation ? touchAccountKeyAt(touch.clientX, touch.clientY) : null;
      if (accountKey) {
        setTouchRevealedAccounts((current) => current.includes(accountKey) ? current : [...current, accountKey]);
      }
    }

    function finish(event: TouchEvent) {
      if (!session) return;
      const touch = touchWithIdentifier(event.changedTouches, session.identifier);
      if (!touch) return;
      if (!session.active) {
        resetTouchDrag();
        return;
      }
      if (event.cancelable) event.preventDefault();
      const dropTarget = session.movedAfterActivation ? touchDropTargetAt(touch.clientX, touch.clientY) : null;
      const messageIDs = session.messageIDs;
      const accountIDs = session.accountIDs;
      resetTouchDrag();
      if (!dropTarget) return;
      const crossAccount = accountIDs.some((accountID) => dropTarget.accountID > 0 && accountID !== dropTarget.accountID);
      onMoveMessagesRef.current(messageIDs, { id: dropTarget.id, name: dropTarget.name }, crossAccount ? "copy" : "move");
    }

    function cancel() {
      resetTouchDrag();
    }

    function suppressContextMenu(event: Event) {
      if (session || suppressCompatibilityClick) event.preventDefault();
    }

    function suppressGeneratedClick(event: Event) {
      if (!suppressCompatibilityClick || !(event instanceof globalThis.MouseEvent)) return;
      if (Math.hypot(event.clientX - compatibilityClickX, event.clientY - compatibilityClickY) > 40) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      suppressCompatibilityClick = false;
      clearCompatibilityClickTimer();
    }

    function suppressNativeTouchDrag(event: Event) {
      if (!session) return;
      event.preventDefault();
      event.stopImmediatePropagation();
    }

    function handleVisibilityChange() {
      if (document.visibilityState !== "visible") resetTouchDrag();
    }

    function attachSessionListeners(source: HTMLElement) {
      if (sessionListenersAttached) return;
      sessionListenersAttached = true;
      sessionEventTarget = source;
      source.addEventListener("touchmove", move, { passive: false });
      source.addEventListener("touchend", finish, { passive: false });
      source.addEventListener("touchcancel", cancel, { passive: true });
    }

    function detachSessionListeners() {
      if (!sessionListenersAttached || !sessionEventTarget) return;
      sessionListenersAttached = false;
      sessionEventTarget.removeEventListener("touchmove", move);
      sessionEventTarget.removeEventListener("touchend", finish);
      sessionEventTarget.removeEventListener("touchcancel", cancel);
      sessionEventTarget = null;
    }

    document.addEventListener("touchstart", start, { passive: true, capture: true });
    document.addEventListener("contextmenu", suppressContextMenu, true);
    document.addEventListener("click", suppressGeneratedClick, true);
    document.addEventListener("dragstart", suppressNativeTouchDrag, true);
    document.addEventListener("visibilitychange", handleVisibilityChange);
    window.addEventListener("blur", cancel);
    return () => {
      clearHoldTimer();
      clearCompatibilityClickTimer();
      clearTouchClassRemovalTimer();
      detachSessionListeners();
      restoreNativeDrag();
      document.documentElement.classList.remove("rolltop-touch-message-dragging");
      document.removeEventListener("touchstart", start, true);
      document.removeEventListener("contextmenu", suppressContextMenu, true);
      document.removeEventListener("click", suppressGeneratedClick, true);
      document.removeEventListener("dragstart", suppressNativeTouchDrag, true);
      document.removeEventListener("visibilitychange", handleVisibilityChange);
      window.removeEventListener("blur", cancel);
    };
  }, []);

  function clearNativeDragActivationTimer() {
    if (nativeDragActivationTimer.current === null) return;
    window.clearTimeout(nativeDragActivationTimer.current);
    nativeDragActivationTimer.current = null;
  }

  function openMobileSidebar() {
    clearNativeDragActivationTimer();
    dragOpenedSidebar.current = false;
    mobileSidebarOpenRef.current = true;
    setMobileSidebarOpen(true);
  }

  function closeMobileSidebar() {
    clearNativeDragActivationTimer();
    dragOpenedSidebar.current = false;
    mobileSidebarOpenRef.current = false;
    setMobileSidebarOpen(false);
  }

  function beginMessageDrag(event: DragEvent<HTMLDivElement>) {
    if (!isRolltopMessageDrag(event)) return;
    clearNativeDragActivationTimer();
    nativeDragInProgress.current = true;
    nativeDragActivationTimer.current = window.setTimeout(() => {
      nativeDragActivationTimer.current = null;
      if (!nativeDragInProgress.current) return;
      setMessageDragActive(true);
      if (!window.matchMedia("(max-width: 760px)").matches || mobileSidebarOpenRef.current) return;
      dragOpenedSidebar.current = true;
      mobileSidebarOpenRef.current = true;
      setMobileSidebarOpen(true);
    }, 0);
  }

  function endMessageDrag(event: DragEvent<HTMLDivElement>) {
    if (!isRolltopMessageDrag(event)) return;
    if (!nativeDragInProgress.current) return;
    nativeDragInProgress.current = false;
    clearNativeDragActivationTimer();
    setMessageDragActive(false);
    if (!dragOpenedSidebar.current) return;
    dragOpenedSidebar.current = false;
    mobileSidebarOpenRef.current = false;
    setMobileSidebarOpen(false);
  }

  function composeFromMobile() {
    closeMobileSidebar();
    openCompose("");
  }

  return (
    <>
      <Topbar
        user={user}
        mailboxes={mailboxes}
        enabledPlugins={enabledPlugins}
        location={location}
        navigate={navigate}
        notificationsEnabled={notificationsEnabled}
        toggleNotifications={toggleNotifications}
        securityUnlockAvailable={securityUnlockAvailable}
        securityUnlock={securityUnlock}
        openSecurityUnlock={openSecurityUnlock}
        lockSecurity={lockSecurity}
        logout={logout}
        onMenu={openMobileSidebar}
      />
      <div
        ref={appRef}
        className={`app ${messageDragActive ? "message-drag-active" : ""}`}
        onDragStart={beginMessageDrag}
        onDragEnd={endMessageDrag}
      >
        {mobileSidebarOpen && !messageDragActive ? (
          <button className="mobile-sidebar-scrim" type="button" aria-label="Close folders" onClick={closeMobileSidebar} />
        ) : null}
        <Sidebar
          key={user.id}
          userID={user.id}
          mailboxes={mailboxes}
          csrf={csrf}
          latestSyncRun={latestSyncRun}
          activeSyncRuns={activeSyncRuns}
          syncRunning={syncRunning}
          serverStartedAt={serverStartedAt}
          serverUptimeSeconds={serverUptimeSeconds}
          buildVersion={buildVersion}
          buildDate={buildDate}
          buildLabel={buildLabel}
          buildCommit={buildCommit}
          currentPath={location.path}
          navigate={navigate}
          openCompose={openCompose}
          refreshChrome={refreshChrome}
          onMoveMessages={onMoveMessages}
          mobileOpen={mobileSidebarOpen}
          dragActive={messageDragActive}
          touchDropID={touchDropID}
          touchRevealedAccounts={touchRevealedAccounts}
          onClose={closeMobileSidebar}
        />
        <main className="content">
          {accountNeedsPassword ? <AccountCredentialBanner notice={accountNotice} navigate={navigate} /> : null}
          {children}
        </main>
        {touchDragPreview ? (
          <div
            className="message-touch-drag-preview"
            style={{ left: touchDragPreview.left, top: touchDragPreview.top }}
            aria-hidden="true"
          >
            <Icon name="mail" weight="bold" />
            <span>{touchDragPreview.count === 1 ? "1 message" : `${touchDragPreview.count.toLocaleString()} messages`}</span>
          </div>
        ) : null}
      </div>
      <button className="mobile-compose-fab" type="button" onClick={composeFromMobile} aria-label="Compose">
        <Icon name="edit" weight="bold" />
        <span>Compose</span>
      </button>
    </>
  );
}

function isRolltopMessageDrag(event: DragEvent<HTMLElement>) {
  const target = event.target;
  if (target instanceof Element && target.closest("[data-rolltop-message-drag]")) return true;
  const types = Array.from(event.dataTransfer.types);
  return types.includes("application/x-rolltop-message-transfer") ||
    types.includes("application/x-rolltop-messages") ||
    types.includes("application/x-rolltop-message");
}

const androidTouchDragHoldMS = 180;
const androidTouchDragSlop = 12;

type TouchDragPreview = {
  left: number;
  top: number;
  count: number;
};

type AndroidTouchDragSession = {
  identifier: number;
  startX: number;
  startY: number;
  lastX: number;
  lastY: number;
  messageIDs: number[];
  accountIDs: number[];
  active: boolean;
  movedAfterActivation: boolean;
  holdTimer: number | null;
};

type TouchDropTarget = {
  id: number;
  name: string;
  accountID: number;
};

function positiveIDs(raw: string | undefined): number[] {
  if (!raw) return [];
  return Array.from(new Set(raw.split(",")
    .map((value) => Number.parseInt(value, 10))
    .filter((value) => Number.isFinite(value) && value > 0)));
}

function touchWithIdentifier(touches: TouchList, identifier: number): Touch | null {
  for (let index = 0; index < touches.length; index += 1) {
    if (touches[index].identifier === identifier) return touches[index];
  }
  return null;
}

function touchDropTargetAt(x: number, y: number): TouchDropTarget | null {
  const target = document.elementFromPoint(x, y)?.closest<HTMLElement>("[data-rolltop-drop-mailbox-id]");
  if (!target) return null;
  const id = Number.parseInt(target.dataset.rolltopDropMailboxId || "", 10);
  if (!Number.isFinite(id) || id <= 0) return null;
  const accountID = Number.parseInt(target.dataset.rolltopDropAccountId || "", 10);
  return {
    id,
    name: target.dataset.rolltopDropMailboxName || "Folder",
    accountID: Number.isFinite(accountID) && accountID > 0 ? accountID : 0
  };
}

function touchAccountKeyAt(x: number, y: number): string | null {
  const target = document.elementFromPoint(x, y)?.closest<HTMLElement>("[data-rolltop-drop-account-key]");
  return target?.dataset.rolltopDropAccountKey || null;
}

function touchPreviewAt(x: number, y: number, count: number): TouchDragPreview {
  const width = 156;
  return {
    left: Math.max(8, Math.min(window.innerWidth - width - 8, x + 16)),
    top: Math.max(72, y - 58),
    count
  };
}

// This banner is intentionally high in the shell so a broken master key or
// undecryptable IMAP password is visible on every authenticated page.
function AccountCredentialBanner({ notice, navigate }: { notice: string; navigate: (url: string) => void }) {
  return (
    <section className="account-alert" role="alert">
      <Icon name="report" weight="duotone" />
      <div>
        <strong>IMAP password required</strong>
        <span>{notice || "The saved IMAP password cannot be decrypted. Re-enter it to restore sync and full-message loading."}</span>
      </div>
      <button type="button" onClick={() => navigate("/settings/account")}>Re-enter password</button>
    </section>
  );
}

function useServerUptimeLabel(startedAt: string, fallbackSeconds: number) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 60_000);
    return () => window.clearInterval(timer);
  }, [startedAt]);

  const started = Date.parse(startedAt || "");
  const seconds = Number.isFinite(started)
    ? Math.max(0, Math.floor((now - started) / 1000))
    : Math.max(0, Math.floor(fallbackSeconds || 0));
  return formatUptime(seconds);
}

function formatUptime(totalSeconds: number) {
  if (!Number.isFinite(totalSeconds) || totalSeconds <= 0) return "";
  const days = Math.floor(totalSeconds / 86_400);
  const hours = Math.floor((totalSeconds % 86_400) / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${Math.max(1, minutes)}m`;
}

function buildDisplayLabel(version: string, buildDate: string, fallbackLabel: string) {
  const trimmedVersion = version.trim();
  if (trimmedVersion && trimmedVersion.toLowerCase() !== "latest") return trimmedVersion;
  const parsed = Date.parse(buildDate || "");
  if (Number.isFinite(parsed)) {
    return `built ${new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" }).format(parsed)}`;
  }
  return fallbackLabel.trim();
}

// Topbar owns the search input because search is global navigation, not part of
// a specific mailbox or message view.
function Topbar({
  user,
  mailboxes,
  enabledPlugins,
  location,
  navigate,
  notificationsEnabled,
  toggleNotifications,
  securityUnlockAvailable,
  securityUnlock,
  openSecurityUnlock,
  lockSecurity,
  logout,
  onMenu
}: {
  user: User;
  mailboxes: Mailbox[];
  enabledPlugins: string[];
  location: LocationState;
  navigate: (url: string) => void;
  notificationsEnabled: boolean;
  toggleNotifications: () => Promise<void>;
  securityUnlockAvailable: boolean;
  securityUnlock: SecurityUnlockState;
  openSecurityUnlock: (identityID?: number, onUnlocked?: (state: SecurityUnlockState) => void, recipientKeyIDs?: string[], fallbackEmail?: string) => void;
  lockSecurity: () => void;
  logout: () => Promise<void>;
  onMenu: () => void;
}) {
  const [query, setQuery] = useState(() => searchRoute(currentLocation().path).query);
  const [focused, setFocused] = useState(false);
  const searchInputRef = useRef<HTMLInputElement>(null);
  const accountMenuRef = useRef<HTMLDetailsElement>(null);
  const pluginKey = enabledPlugins.join("|");
  const pluginSet = useMemo(() => createPluginSet(enabledPlugins), [pluginKey]);
  const securityUnlocked = securityUnlock.keys.length > 0 && securityUnlock.unlockedUntil > Date.now();
  const autocomplete = useSearchAutocomplete({
    query,
    focused,
    inputRef: searchInputRef,
    mailboxes,
    pluginSet,
    setQuery
  });

  useEffect(() => {
    setQuery(searchRoute(location.path).query);
  }, [location.path]);

  useEffect(() => {
    function focusSearch(event: KeyboardEvent) {
      if (event.key !== "/" || shouldIgnoreMailShortcut(event)) return;
      event.preventDefault();
      searchInputRef.current?.focus();
      searchInputRef.current?.select();
    }
    window.addEventListener("keydown", focusSearch);
    return () => window.removeEventListener("keydown", focusSearch);
  }, []);

  function submit(event: FormEvent) {
    event.preventDefault();
    const trimmed = query.trim();
    if (trimmed === "") {
      navigate("/mail");
      return;
    }
    navigate(searchURL(trimmed));
  }

  function closeAccountMenu() {
    if (accountMenuRef.current) accountMenuRef.current.open = false;
  }

  function menuNavigate(url: string) {
    closeAccountMenu();
    navigate(url);
  }

  async function menuToggleNotifications() {
    await toggleNotifications();
    closeAccountMenu();
  }

  async function menuLogout() {
    closeAccountMenu();
    await logout();
  }

  const accountLabel = user.name || user.email;

  return (
    <header className="topbar">
      <button className="ghost mobile-menu-button" type="button" title="Folders" aria-label="Folders" onClick={onMenu}>
        <Icon name="menu" />
      </button>
      <a
        href="/mail"
        className="brand"
        onClick={(event) => {
          event.preventDefault();
          navigate("/mail");
        }}
      >
        <LogoMark />
        <span className="brand-wordmark">rolltop</span>
      </a>
      <form className="top-search" onSubmit={submit}>
        <Icon name="search" />
        <input
          ref={searchInputRef}
          type="search"
          placeholder="Search mail"
          value={query}
          onFocus={() => setFocused(true)}
          onBlur={() => window.setTimeout(() => setFocused(false), 120)}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={autocomplete.onKeyDown}
          autoComplete="off"
        />
        {focused ? <SearchAutocomplete items={autocomplete.items} activeIndex={autocomplete.activeIndex} onChoose={autocomplete.choose} /> : null}
      </form>
      <nav className="top-actions" aria-label="Account">
        {securityUnlockAvailable ? (
          <button
            className={securityUnlocked ? "ghost security-lock-toggle active" : "ghost security-lock-toggle"}
            type="button"
            title={securityUnlocked ? "Lock security keys" : "Unlock security key"}
            onClick={securityUnlocked ? lockSecurity : () => openSecurityUnlock()}
          >
            <Icon name={securityUnlocked ? "lock_open" : "lock"} weight={securityUnlocked ? "bold" : "regular"} />
          </button>
        ) : null}
        <details className="account-menu" ref={accountMenuRef}>
          <summary className="user-chip account-menu-summary" title={accountLabel} aria-label="Account menu">
            <span>{accountLabel}</span>
            <Icon name="expand_more" />
          </summary>
          <div className="account-menu-panel" role="menu">
            <div className="account-menu-identity">
              <strong>{accountLabel}</strong>
              <small>{user.email}</small>
            </div>
            {!androidNativeAvailable() ? (
              <button
                className={notificationsEnabled ? "account-menu-row account-menu-notifications active" : "account-menu-row account-menu-notifications"}
                type="button"
                role="switch"
                aria-checked={notificationsEnabled}
                onClick={() => void menuToggleNotifications()}
              >
                <Icon name="notifications" weight={notificationsEnabled ? "bold" : "regular"} />
                <span><strong>Browser notifications</strong><small>{notificationsEnabled ? "Enabled for new mail" : "Paused for this browser"}</small></span>
                <span className="notification-toggle-track"><span /></span>
              </button>
            ) : null}
            <button className="account-menu-row" type="button" role="menuitem" onClick={() => menuNavigate("/settings/account")}>
              <Icon name="settings" />
              <span><strong>Settings</strong><small>Profile, servers, folders, and identities</small></span>
            </button>
            {user.is_admin ? (
              <button className="account-menu-row" type="button" role="menuitem" onClick={() => menuNavigate("/admin/users")}>
                <Icon name="group" />
                <span><strong>Admin panel</strong><small>Users and server-wide controls</small></span>
              </button>
            ) : null}
            <button className="account-menu-row danger" type="button" role="menuitem" onClick={() => void menuLogout()}>
              <Icon name="logout" />
              <span><strong>Log out</strong><small>End this browser session</small></span>
            </button>
          </div>
        </details>
      </nav>
    </header>
  );
}

// Sidebar turns flat mailbox summaries into a tree, supports folder navigation,
// and accepts dragged message IDs from the message list.
function Sidebar({
  userID,
  mailboxes,
  csrf,
  latestSyncRun,
  activeSyncRuns,
  syncRunning,
  serverStartedAt,
  serverUptimeSeconds,
  buildVersion,
  buildDate,
  buildLabel,
  buildCommit,
  currentPath,
  navigate,
  openCompose,
  refreshChrome,
  onMoveMessages,
  mobileOpen,
  dragActive,
  touchDropID,
  touchRevealedAccounts,
  onClose
}: {
  userID: number;
  mailboxes: Mailbox[];
  csrf: string;
  latestSyncRun: SyncRun | null;
  activeSyncRuns: SyncRun[];
  syncRunning: boolean;
  serverStartedAt: string;
  serverUptimeSeconds: number;
  buildVersion: string;
  buildDate: string;
  buildLabel: string;
  buildCommit: string;
  currentPath: string;
  navigate: (url: string) => void;
  openCompose: (query?: string) => void;
  refreshChrome: () => Promise<Bootstrap | null>;
  onMoveMessages: (messageIDs: number[], mailbox: MoveTarget, action?: MessageTransferAction) => void;
  mobileOpen: boolean;
  dragActive: boolean;
  touchDropID: number | null;
  touchRevealedAccounts: string[];
  onClose: () => void;
}) {
  const [dropID, setDropID] = useState<number | null>(null);
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(() => new Set());
  const [collapsedAccounts, setCollapsedAccounts] = useState<Set<string>>(() => loadCollapsedAccounts(userID));
  const [dragRevealedAccounts, setDragRevealedAccounts] = useState<Set<string>>(() => new Set());
  const uptimeLabel = useServerUptimeLabel(serverStartedAt, serverUptimeSeconds);
  const releaseLabel = buildDisplayLabel(buildVersion, buildDate, buildLabel);
  const uptimeParts = [uptimeLabel ? `Up ${uptimeLabel}` : "", releaseLabel].filter(Boolean);
  const shortCommit = buildCommit.trim().slice(0, 8);
  const uptimeTitle = [
    serverStartedAt ? `Started ${new Date(serverStartedAt).toLocaleString()}` : "Server uptime",
    shortCommit ? `Commit ${shortCommit}` : ""
  ].filter(Boolean).join(" · ");
  const activeMailbox = mailRoute(currentPath).mailboxID;
  const allMailActive = (currentPath === "/mail" || currentPath.startsWith("/mail/")) && !activeMailbox;
  const snoozedActive = currentPath === "/snoozes";
  const accountGroups = useMemo(() => sidebarAccountGroups(mailboxes), [mailboxes]);
  const advertiseAndroidApp = shouldAdvertiseAndroidApp();

  useEffect(() => {
    if (dragActive) return;
    setDropID(null);
    setDragRevealedAccounts((current) => current.size > 0 ? new Set() : current);
  }, [dragActive]);

  useEffect(() => {
    saveCollapsedAccounts(userID, collapsedAccounts);
  }, [userID, collapsedAccounts]);

  function open(event: MouseEvent, url: string) {
    event.preventDefault();
    navigate(url);
    onClose();
  }

  function canAcceptDraggedMessages(event: DragEvent) {
    const types = Array.from(event.dataTransfer.types);
    return types.includes("application/x-rolltop-message-transfer") || types.includes("application/x-rolltop-messages") || types.includes("application/x-rolltop-message");
  }

  function onDragEnter(event: DragEvent, mailboxID: number) {
    if (!canAcceptDraggedMessages(event)) return;
    event.preventDefault();
    setDropID(mailboxID);
  }

  function dragCopyRequested(event: DragEvent) {
    return event.ctrlKey || event.metaKey || event.dataTransfer.dropEffect === "copy";
  }

  function onDragOver(event: DragEvent, mailboxID: number) {
    if (!canAcceptDraggedMessages(event)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = dragCopyRequested(event) ? "copy" : "move";
    setDropID(mailboxID);
  }

  function onDragLeave(event: DragEvent, mailboxID: number) {
    const nextTarget = event.relatedTarget;
    if (nextTarget instanceof Node && event.currentTarget.contains(nextTarget)) return;
    setDropID((current) => current === mailboxID ? null : current);
  }

  function onDrop(event: DragEvent, mailbox: Mailbox) {
    event.preventDefault();
    setDropID(null);
    const transfer = event.dataTransfer.getData("application/x-rolltop-message-transfer");
    const bulk = event.dataTransfer.getData("application/x-rolltop-messages");
    let ids: number[] = [];
    let sourceAccountIDs: number[] = [];
    if (transfer) {
      try {
        const parsed = JSON.parse(transfer) as { ids?: unknown; account_ids?: unknown };
        if (Array.isArray(parsed.ids)) ids = parsed.ids.map((id) => Number(id)).filter((id) => Number.isFinite(id) && id > 0);
        if (Array.isArray(parsed.account_ids)) {
          sourceAccountIDs = parsed.account_ids.map((id) => Number(id)).filter((id) => Number.isFinite(id) && id > 0);
        }
      } catch {
        ids = [];
        sourceAccountIDs = [];
      }
    }
    if (ids.length === 0 && bulk) {
      try {
        const parsed = JSON.parse(bulk) as unknown;
        if (Array.isArray(parsed)) ids = parsed.map((id) => Number(id)).filter((id) => Number.isFinite(id) && id > 0);
      } catch {
        ids = [];
      }
    }
    if (ids.length === 0) {
      const raw = event.dataTransfer.getData("application/x-rolltop-message") || event.dataTransfer.getData("text/plain");
      const messageID = Number.parseInt(raw, 10);
      if (Number.isFinite(messageID) && messageID > 0) ids = [messageID];
    }
    if (ids.length > 0) {
      const crossAccount = sourceAccountIDs.some((accountID) => mailbox.account_id > 0 && accountID !== mailbox.account_id);
      onMoveMessages(ids, { id: mailbox.id, name: mailbox.name }, crossAccount || dragCopyRequested(event) ? "copy" : "move");
      onClose();
    }
  }

  function toggleGroup(key: string) {
    setExpandedGroups((current) => {
      const next = new Set(current);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  function setAccountCollapsed(key: string, collapsed: boolean) {
    setCollapsedAccounts((current) => {
      if (current.has(key) === collapsed) return current;
      const next = new Set(current);
      if (collapsed) next.add(key);
      else next.delete(key);
      return next;
    });
  }

  // Dragging over a collapsed account reveals its folders for the length of the
  // drag only, so an incidental pass over a header never rewrites the saved layout.
  function revealAccountForDrag(key: string) {
    setDragRevealedAccounts((current) => current.has(key) ? current : new Set(current).add(key));
  }

  // Collapse is the stored preference; the active mailbox and an in-flight drag
  // reveal a group for display without touching what the user saved.
  function accountCollapsed(group: SidebarAccountGroup): boolean {
    if (!collapsedAccounts.has(group.key)) return false;
    if (dragRevealedAccounts.has(group.key) || touchRevealedAccounts.includes(group.key)) return false;
    return !group.folders.some((node) => nodeContainsMailbox(node, activeMailbox));
  }

  function folderLink(mailbox: Mailbox, label = mailbox.name, depth = 0) {
    const active = currentPath.startsWith("/mailbox/") && activeMailbox === String(mailbox.id);
    const count = mailbox.unread_count;
    const url = mailURL(mailbox.id);
    return (
      <a
        href={url}
        className={`folder message-drop-target ${depth > 0 ? "folder-child" : ""} ${active ? "active" : ""} ${dropID === mailbox.id || touchDropID === mailbox.id ? "drop-target" : ""}`}
        data-rolltop-drop-mailbox-id={mailbox.id}
        data-rolltop-drop-mailbox-name={mailbox.name}
        data-rolltop-drop-account-id={mailbox.account_id}
        style={depth > 0 ? { paddingLeft: `${18 + depth * 18}px` } : undefined}
        key={mailbox.id}
        onClick={(event) => open(event, url)}
        onDragEnter={(event) => onDragEnter(event, mailbox.id)}
        onDragOver={(event) => onDragOver(event, mailbox.id)}
        onDragLeave={(event) => onDragLeave(event, mailbox.id)}
        onDrop={(event) => onDrop(event, mailbox)}
      >
        <span className="folder-name"><Icon name={mailbox.icon || "folder"} weight={active ? "bold" : undefined} />{label}</span>
        {count > 0 ? <span className="folder-count">{count.toLocaleString()}</span> : null}
      </a>
    );
  }

  function folderNode(node: FolderNode, depth = 0): ReactNode {
    if (node.children.length === 0) return folderLink(node.mailbox, node.label, depth);
    const active = currentPath.startsWith("/mailbox/") && activeMailbox === String(node.mailbox.id);
    const count = node.mailbox.unread_count;
    const expandKey = folderExpandKey(node.mailbox);
    const expanded = expandedGroups.has(expandKey) || nodeContainsMailbox(node, activeMailbox);
    const url = mailURL(node.mailbox.id);
    return (
      <div className="folder-tree" key={node.mailbox.id}>
        <div
          className={`folder folder-parent message-drop-target ${depth > 0 ? "folder-child" : ""} ${active ? "active" : ""} ${dropID === node.mailbox.id || touchDropID === node.mailbox.id ? "drop-target" : ""}`}
          data-rolltop-drop-mailbox-id={node.mailbox.id}
          data-rolltop-drop-mailbox-name={node.mailbox.name}
          data-rolltop-drop-account-id={node.mailbox.account_id}
          style={depth > 0 ? { paddingLeft: `${18 + depth * 18}px` } : undefined}
          onDragEnter={(event) => onDragEnter(event, node.mailbox.id)}
          onDragOver={(event) => onDragOver(event, node.mailbox.id)}
          onDragLeave={(event) => onDragLeave(event, node.mailbox.id)}
          onDrop={(event) => onDrop(event, node.mailbox)}
        >
          <a href={url} className="folder-main" onClick={(event) => open(event, url)}>
            <span className="folder-name"><Icon name={node.mailbox.icon || "folder"} weight={active ? "bold" : undefined} />{node.label}</span>
          </a>
          {count > 0 ? <span className="folder-count">{count.toLocaleString()}</span> : null}
          <button className="folder-toggle" type="button" onClick={() => toggleGroup(expandKey)} title={expanded ? "Collapse folder" : "Expand folder"}>
            <Icon name={expanded ? "expand_more" : "chevron_right"} />
          </button>
        </div>
        {expanded ? <div className="folder-children">{node.children.map((child) => folderNode(child, depth + 1))}</div> : null}
      </div>
    );
  }

  return (
    <aside className={`sidebar ${mobileOpen ? "open" : ""} ${dragActive ? "message-drag-active" : ""}`}>
      <div className="sidebar-mobile-head">
        <a
          href="/mail"
          className="brand sidebar-mobile-brand"
          aria-label="Rolltop - All Mail"
          onClick={(event) => open(event, "/mail")}
        >
          <LogoMark />
          <span className="brand-wordmark">rolltop</span>
        </a>
        <button className="ghost" type="button" title="Close folders" aria-label="Close folders" onClick={onClose}><Icon name="close" /></button>
      </div>
      <a href="/compose" className="button compose" onClick={(event) => {
        event.preventDefault();
        onClose();
        openCompose("");
      }}>
        <Icon name="edit" weight="bold" />
        Compose
      </a>
      <div className="sidebar-scroll">
        <a
          href="/mail"
          className={`folder ${allMailActive ? "active" : ""}`}
          onClick={(event) => open(event, "/mail")}
        >
          <span className="folder-name"><Icon name="mail" weight={allMailActive ? "bold" : undefined} />All Mail</span>
        </a>
    <a
      href="/snoozes"
      className={`folder ${snoozedActive ? "active" : ""}`}
      onClick={(event) => open(event, "/snoozes")}
    >
      <span className="folder-name"><Icon name="clock" weight={snoozedActive ? "bold" : undefined} />Snoozed</span>
    </a>
        <div className="side-section">Folders</div>
        {accountGroups.map((group) => {
          const collapsed = accountCollapsed(group);
          const unread = collapsed ? folderTreeUnreadCount(group.folders) : 0;
          return (
            <div className="account-folder-group" key={group.key}>
              <button
                type="button"
                className="account-toggle"
                aria-expanded={!collapsed}
                title={collapsed ? "Expand account folders" : "Collapse account folders"}
                data-rolltop-drop-account-key={group.key}
                onClick={() => setAccountCollapsed(group.key, !collapsed)}
                onDragEnter={(event) => {
                  if (collapsed && canAcceptDraggedMessages(event)) revealAccountForDrag(group.key);
                }}
              >
                <Icon name={collapsed ? "chevron_right" : "expand_more"} />
                <span className="account-toggle-label">{group.label}</span>
                {unread > 0 ? <span className="folder-count">{unread.toLocaleString()}</span> : null}
              </button>
              {collapsed ? null : group.folders.map((node) => folderNode(node))}
            </div>
          );
        })}
        <div className="side-section">Address Book</div>
        <a
          href="/contacts"
          className={`folder ${currentPath === "/contacts" ? "active" : ""}`}
          onClick={(event) => open(event, "/contacts")}
        >
          <span className="folder-name"><Icon name="group" weight={currentPath === "/contacts" ? "bold" : undefined} />Contacts</span>
        </a>
        {advertiseAndroidApp ? (
          <>
            <div className="side-section">Android app</div>
            <a href="/android/rolltop.apk" className="folder android-app-download" download="rolltop.apk">
              <span className="folder-name"><Icon name="android" weight="fill" />Get Rolltop for Android</span>
              <Icon name="download" />
            </a>
          </>
        ) : null}
      </div>
      <SidebarSync mailboxes={mailboxes} csrf={csrf} latest={latestSyncRun} activeRuns={activeSyncRuns} running={syncRunning} refreshChrome={refreshChrome} />
      {uptimeParts.length > 0 ? (
        <div className="sidebar-uptime" title={uptimeTitle}>
          {uptimeParts.join(" · ")}
        </div>
      ) : null}
      <div className="sidebar-license">
        GNU AGPLv3-or-later
      </div>
    </aside>
  );
}

type SidebarAccountGroup = {
  key: string;
  label: string;
  folders: FolderNode[];
};

function sidebarAccountGroups(mailboxes: Mailbox[]): SidebarAccountGroup[] {
  const grouped = new Map<string, { key: string; label: string; mailboxes: Mailbox[] }>();
  for (const mailbox of mailboxes) {
    const key = mailbox.account_id ? String(mailbox.account_id) : `email:${mailbox.account_email || "local"}`;
    const existing = grouped.get(key);
    if (existing) {
      existing.mailboxes.push(mailbox);
      continue;
    }
    grouped.set(key, { key, label: mailboxAccountLabel(mailbox), mailboxes: [mailbox] });
  }

  const groups = Array.from(grouped.values());
  const labelCounts = groups.reduce((counts, group) => {
    counts.set(group.label, (counts.get(group.label) || 0) + 1);
    return counts;
  }, new Map<string, number>());

  return groups
    .map((group) => ({
      key: group.key,
      label: (labelCounts.get(group.label) || 0) > 1 ? `${group.label} · Account ${group.key}` : group.label,
      folders: folderTree(group.mailboxes)
    }))
    .filter((group) => group.folders.length > 0);
}

function mailboxAccountLabel(mailbox: Mailbox): string {
  const label = (mailbox.account_label || mailbox.account_email || "").trim();
  if (label) return label;
  return mailbox.account_id ? `Account ${mailbox.account_id}` : "Mail account";
}

function folderExpandKey(mailbox: Mailbox): string {
  return `${mailbox.account_id}:${mailbox.name}`;
}

function SidebarSync({
  mailboxes,
  csrf,
  latest,
  activeRuns,
  running,
  refreshChrome
}: {
  mailboxes: Mailbox[];
  csrf: string;
  latest: SyncRun | null;
  activeRuns: SyncRun[];
  running: boolean;
  refreshChrome: () => Promise<Bootstrap | null>;
}) {
  const [busy, setBusy] = useState(false);
  const orderedActiveRuns = useMemo(() => stableSyncRunOrder(activeRuns), [activeRuns]);
  // Historical runs belong on the settings history page. The sidebar is live
  // activity only: falling back to the latest interrupted/no-op row made a
  // settled mailbox look as though it were perpetually syncing.
  // IMAP STATUS probes are similarly background housekeeping. They can fan out
  // across every folder while a remote import is active, but they neither copy
  // mail nor need attention, so keep them out of the chrome altogether.
  const visibleRuns = orderedActiveRuns.filter((run) => !isSyncRunChecking(run));
  const isActive = visibleRuns.length > 0 || (running && activeRuns.length === 0);
  const controlsBusy = busy || running || activeRuns.length > 0;
  const latestVisible = visibleRuns[visibleRuns.length - 1] || null;

  async function startSync() {
    setBusy(true);
    try {
      await api.syncAccount(csrf);
      await refreshChrome();
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className={`sidebar-sync ${isActive ? "running" : "idle"}`}>
      <div className="sync-meta">
        <strong>{isActive ? `Syncing${visibleRuns.length > 1 ? ` (${visibleRuns.length})` : ""}` : "Sync"}</strong>
        <span>{isActive ? (latestVisible ? `${latestVisible.status}${latestVisible.current_mailbox ? ` - ${latestVisible.current_mailbox}` : ""}` : "starting") : "Up to date"}</span>
        <button className="secondary" type="button" disabled={controlsBusy} onClick={startSync}>
          <Icon name="sync" />
			{controlsBusy ? "Syncing" : "Sync now"}
        </button>
      </div>
      <div className="sync-run-list">
        {visibleRuns.map((run) => (
          <SyncRunMini key={run.id} run={run} mailbox={syncRunMailbox(run, mailboxes)} />
        ))}
      </div>
    </section>
  );
}

// A run is created before IMAP STATUS has determined whether there are any
// UIDs to fetch. Calling that short no-op phase "Syncing" made an unchanged
// INBOX look permanently busy, especially with two IMAP accounts.
function isSyncRunChecking(run: SyncRun): boolean {
	return run.status === "running" && run.messages_total === 0 && run.messages_stored === 0 &&
		run.latest_new_from !== "rolltop:maintenance" && run.latest_new_from !== "rolltop:move";
}


function stableSyncRunOrder(runs: SyncRun[]) {
  return [...runs].sort((a, b) => {
    const startedA = Date.parse(a.started_at) || 0;
    const startedB = Date.parse(b.started_at) || 0;
    if (startedA !== startedB) return startedA - startedB;
    return a.id - b.id;
  });
}

function syncRunMailbox(run: SyncRun, mailboxes: Mailbox[]): Mailbox | undefined {
  const name = run.current_mailbox.trim().toLowerCase();
  if (!name) return undefined;
  return mailboxes.find((mailbox) =>
    mailbox.account_id === run.account_id && mailbox.name.trim().toLowerCase() === name
  );
}

type MirrorResumeEstimate = {
  completed: number;
  total: number;
  approximate: boolean;
};

// Mailbox counts and UID checkpoints survive a process restart, unlike the
// short-lived sync run. Prefer exact IMAP/local counts; UID range is a clearly
// labelled fallback because IMAP UIDs can have gaps.
function mirrorResumeEstimate(mailbox: Mailbox | undefined): MirrorResumeEstimate | null {
  if (!mailbox) return null;
  const local = Math.max(0, mailbox.local_message_count ?? mailbox.message_count);
  const remote = Math.max(0, mailbox.remote_message_count);
  if (remote > 0) {
    return { completed: Math.min(local, remote), total: remote, approximate: false };
  }
  const uidTotal = Math.max(0, mailbox.remote_uid_next - 1);
  if (uidTotal <= 0) return null;
  return { completed: Math.min(Math.max(0, mailbox.last_uid), uidTotal), total: uidTotal, approximate: true };
}

/** Render current-run activity alongside the durable local mirror checkpoint. */
export function SyncRunMini({ run, mailbox }: { run: SyncRun; mailbox?: Mailbox }) {
	const isChecking = isSyncRunChecking(run);
  const totalMessages = run.messages_total || 0;
  const totalFolders = run.mailboxes_total || 0;
  const runProgress = totalMessages > 0
    ? Math.min(100, Math.round((run.messages_seen / totalMessages) * 100))
      : totalFolders > 0
        ? Math.min(100, Math.round((run.mailboxes_done / totalFolders) * 100))
        : run.status === "running" ? 100 : 0;
  const isPurge = run.latest_new_from === "rolltop:maintenance" && run.latest_new_subject.trim().toLowerCase().startsWith("purging");
  const isMove = run.latest_new_from === "rolltop:move";
  const isMaintenance = run.latest_new_from === "rolltop:maintenance";
	const resume = run.status === "running" && !isChecking && !isPurge && !isMove && !isMaintenance ? mirrorResumeEstimate(mailbox) : null;
  const resumePercent = resume && resume.total > 0 ? Math.round((resume.completed * 100) / resume.total) : 0;
  const progress = resume ? resumePercent : runProgress;
	const processedLabel = run.messages_stored > 0 ? `${run.messages_stored.toLocaleString()} processed` : isChecking ? "Checking IMAP status..." : "Syncing...";
  const resumeLabel = resume
    ? resume.approximate
      ? `about ${resume.completed.toLocaleString()} of ${resume.total.toLocaleString()} already mirrored`
      : `${resume.completed.toLocaleString()} of ${resume.total.toLocaleString()} already mirrored`
    : "";
  const movedLabel = totalMessages > 0
    ? `${run.messages_seen.toLocaleString()} of ${totalMessages.toLocaleString()} moved`
    : "Moving...";
  const purgeLabel = totalMessages > 0
    ? `${run.messages_seen.toLocaleString()} of ${totalMessages.toLocaleString()} purged`
    : "Purging...";
  const detail = isPurge
    ? purgeLabel
    : isMove
      ? movedLabel
      : run.messages_skipped > 0
        ? `${processedLabel}, ${run.messages_skipped.toLocaleString()} skipped`
        : processedLabel;
  return (
    <div className="sync-run-mini">
      <div className="sync-run-title">
        <span>{run.current_mailbox || run.status}</span>
        <span>{progress}%</span>
      </div>
      <div className="sync-run-detail">{resumeLabel ? `${resumeLabel} · ${detail}` : detail}</div>
      <div className="progress" aria-label={`${run.current_mailbox || "Sync"} progress`}>
        <div style={{ width: `${progress}%` }} />
      </div>
    </div>
  );
}
