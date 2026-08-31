// File overview: Small route switch for the single-page app. It translates the parsed location
// into feature views while passing only the shared state each view needs.

import type { AddToast, LocationState, SecurityUnlockState } from "./appTypes";
import type { Bootstrap, Mailbox, OutboxSummary, SwipePreferences, SyncRun, ThemeDefinition, User } from "./types";
import { MailView, SearchView, SnoozedView } from "./features/mail/MailViews";
import { ThreadView } from "./features/mail/ThreadView";
import { ComposePage } from "./features/compose/ComposeViews";
import { ContactsView } from "./features/contacts/ContactsView";
import { OutboxView } from "./features/outbox/OutboxView";
import { SettingsView, AdminUsersView, SyncRunView } from "./features/settings/SettingsViews";
import type { RuntimePlugins } from "./plugins/runtime";
import { securityUnlockPlugin } from "./plugins/securityUnlock";

/**
 * RouteView is the app's manual router. Each branch maps one URL family to a
 * feature view and passes shared chrome state downward without letting features
 * import App-level bootstrap or navigation state directly.
 */
export function RouteView({
  csrf,
  user,
  mailboxes,
  latestSyncRun,
  activeSyncRuns,
  syncRunning,
  outbox,
  mailGeneration,
  swipePreferences,
  enabledPlugins,
  availableThemes,
  location,
  navigate,
  hiddenMessageIDs,
  openCompose,
  refreshChrome,
  runtimePlugins,
  reloadRuntimePlugins,
  securityUnlock,
  openSecurityUnlock,
  addToast
}: {
  csrf: string;
  user: User;
  mailboxes: Mailbox[];
  latestSyncRun: SyncRun | null;
  activeSyncRuns: SyncRun[];
  syncRunning: boolean;
  outbox: OutboxSummary;
  mailGeneration: number;
  swipePreferences: SwipePreferences;
  enabledPlugins: string[];
  availableThemes: ThemeDefinition[];
  location: LocationState;
  navigate: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  openCompose: (query?: string) => void;
  refreshChrome: () => Promise<Bootstrap | null>;
  runtimePlugins: RuntimePlugins;
  reloadRuntimePlugins: () => Promise<void>;
  securityUnlock: SecurityUnlockState;
  openSecurityUnlock: (identityID?: number, onUnlocked?: (state: SecurityUnlockState) => void, recipientKeyIDs?: string[], fallbackEmail?: string) => void;
  addToast: AddToast;
}) {
  const securityEnabled = Boolean(securityUnlockPlugin(runtimePlugins.all));
  if (location.path === "/snoozes") {
    return <SnoozedView csrf={csrf} datePrefs={user} location={location} navigate={navigate} hiddenMessageIDs={hiddenMessageIDs} mailboxes={mailboxes} swipePreferences={swipePreferences} mailGeneration={mailGeneration} messageSecurityPlugins={runtimePlugins.all} addToast={addToast} />;
  }
  if (location.path === "/search" || location.path.startsWith("/search/")) {
    return <SearchView csrf={csrf} userID={user.id} location={location} navigate={navigate} hiddenMessageIDs={hiddenMessageIDs} datePrefs={user} mailboxes={mailboxes} swipePreferences={swipePreferences} activeSyncRuns={activeSyncRuns} messageSecurityPlugins={runtimePlugins.all} searchActionPlugins={runtimePlugins.all} addToast={addToast} />;
  }
  if (location.path.startsWith("/messages/")) {
    return (
      <ThreadView
        userID={user.id}
        csrf={csrf}
        datePrefs={user}
        location={location}
        navigate={navigate}
        mailboxes={mailboxes}
        enabledPlugins={enabledPlugins}
        refreshChrome={refreshChrome}
        openCompose={openCompose}
        messageSecurityPlugins={runtimePlugins.all}
        securityUnlock={securityUnlock}
        openSecurityUnlock={openSecurityUnlock}
        addToast={addToast}
      />
    );
  }
  if (location.path === "/compose") {
    return <ComposePage userID={user.id} csrf={csrf} location={location} navigate={navigate} securityEnabled={securityEnabled} securityPlugins={runtimePlugins.all} securityUnlock={securityUnlock} openSecurityUnlock={openSecurityUnlock} addToast={addToast} />;
  }
  if (location.path === "/outbox") {
    return <OutboxView csrf={csrf} summary={outbox} navigate={navigate} addToast={addToast} />;
  }
  if (location.path === "/contacts") {
    return <ContactsView csrf={csrf} contactPlugins={runtimePlugins.all} navigate={navigate} openCompose={openCompose} addToast={addToast} />;
  }
  if (location.path === "/settings/account" || location.path.startsWith("/settings/account/")) {
    return <SettingsView key={user.id} csrf={csrf} user={user} mailboxes={mailboxes} swipePreferences={swipePreferences} latestSyncRun={latestSyncRun} activeSyncRuns={activeSyncRuns} syncRunning={syncRunning} availableThemes={availableThemes} location={location} navigate={navigate} refreshChrome={refreshChrome} runtimePlugins={runtimePlugins} reloadRuntimePlugins={reloadRuntimePlugins} addToast={addToast} />;
  }
  if (location.path === "/admin/users" && user.is_admin) {
    return <AdminUsersView csrf={csrf} refreshChrome={refreshChrome} addToast={addToast} />;
  }
  if (location.path.startsWith("/sync-runs/")) {
    return <SyncRunView csrf={csrf} location={location} navigate={navigate} datePrefs={user} />;
  }
  return (
    <MailView
      userID={user.id}
      csrf={csrf}
      datePrefs={user}
      location={location}
      navigate={navigate}
      hiddenMessageIDs={hiddenMessageIDs}
      mailboxes={mailboxes}
      latestSyncRun={latestSyncRun}
      activeSyncRuns={activeSyncRuns}
      mailGeneration={mailGeneration}
      swipePreferences={swipePreferences}
      refreshChrome={refreshChrome}
      addToast={addToast}
      messageSecurityPlugins={runtimePlugins.all}
    />
  );
}
