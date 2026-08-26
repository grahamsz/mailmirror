# Rolltop Android

This is a native Android wrapper for a Rolltop server. It gives Rolltop an installable Android app surface with WebView session persistence, background push notifications, Android contacts/file pickers, default mail app registration intents, Android share targets, and a server-driven update prompt flow.

## Build

```sh
./android/gradlew -p android :app:assembleDebug
```

CI uploads a release APK when signing secrets are configured; unsigned environments upload a debug APK for testing only.

## First Run

Enter the HTTPS URL for the Rolltop server, then sign in through the embedded WebView. Shared preferences hold the normalized server URL plus local route, notification-cursor, and update state. Credentials are not copied there; Rolltop session cookies remain in Android WebView cookie storage.

## Default Mail App

The app declares `mailto:` handlers and uses `RoleManager.ROLE_EMAIL` on Android 10 and newer to request the default mail app role. Older Android versions fall back to the system default-apps settings screen.

Rolltop still does not send SMTP mail from the Android shell. Compose actions are routed to the Rolltop `/compose` page.

## Share Targets

Android `ACTION_SEND`, `ACTION_SEND_MULTIPLE`, `ACTION_SENDTO`, and `mailto:` links open Rolltop compose with the available subject/body fields prefilled. Shared photos and files are streamed from their Android content URIs into the existing compose attachment pipeline, so they appear as real removable attachments and use the normal multipart send/draft APIs. Multiple streams and `ClipData` are supported.

The stream bridge is exposed only to the configured Rolltop HTTPS origin. It uses random, short-lived URLs intercepted for both page and service-worker requests inside the WebView; shared bodies are not logged or saved as separate attachment blobs by the Android shell.

The ordinary compose attachment and inline-photo buttons use Android's system file chooser as well.

## Android Contacts

Recipient fields always show a contact-picker button in the Android app. It opens the system email contact picker and returns only the address the user selected, without requiring broad contacts access.

Typed recipient suggestions initially come from Rolltop's saved contacts and recent correspondents for the current user. Recent addresses remain suggestions and are not imported into the address book. The compose suggestion panel also offers an explicit **Enable Android contacts** action. Only that user action requests Android's runtime `READ_CONTACTS` permission. Once granted, each typed query uses Android's email contact filter locally, caps the result set, and merges it with Rolltop suggestions by email address. Device contacts are not imported, persisted, or uploaded to the server. If access is denied, server suggestions and the single-address system picker continue to work.

## Notifications

Rolltop uses UnifiedPush for immediate background wakeups. An installed external distributor is preferred; on devices with Google Play Services, the embedded FCM distributor is the fallback. The embedded distributor accepts Rolltop's existing standards-based VAPID Web Push requests, so self-hosted servers do not need a Firebase project, `google-services.json`, or an FCM service-account secret.

An IMAP `IDLE` wake starts the normal incremental INBOX sync. Each newly stored arrival enters a durable, user-scoped event feed, and the server sends a wake to every registered endpoint. Due snoozes use a separate durable reminder feed and cursor. The Android receiver immediately enqueues unique WorkManager work, then fetches the authenticated feeds with the WebView session cookie. Push bodies are never trusted as account state.

The app also keeps a persistent, network-constrained 15-minute poll as a fallback for a missing distributor, delayed push service, or transient delivery failure. Its cursor is bound to the configured server and authenticated user. First registration establishes a silent baseline before publishing the endpoint, so existing unread mail is never replayed as thousands of new messages. Device endpoints are rotated when the server or signed-in user changes and are unregistered during logout.

A single arrival or reminder shows its sender and subject and opens that message. Multiple events use an expanded inbox-style notification and open All Mail. Single-message alerts offer Reply; alerts whose complete message set is present offer Mark read. Actions and destinations are bound to the normalized server and authenticated user that created the notification, and stale alerts are canceled on logout or server change. Before posting, the app makes a bounded best-effort warm-up request: message bodies use the side-effect-free `/api/messages/:id/prefetch` route, while multiple-message alerts warm the first All Mail page. Prefetching never marks a message read or queues an IMAP `\Seen` update.

Android 13 and newer require the runtime notification permission.

Push registration and polling rely on the WebView login cookie for the configured Rolltop server. If the session expires, the app stops notifying until the user signs in again. The Rolltop server also needs outbound HTTPS access to the selected distributor endpoint.

## Offline Mail

The Android shell inherits the PWA offline behavior. The service worker caches a neutral, session-free app document, so reopening the app without signal boots into cached mail instead of an offline page: everything viewed in the last seven days is available (bodies for conversations that were opened, headers otherwise), search works over that downloaded corpus, and compose sends made offline wait in a sidebar Outbox and deliver automatically when connectivity returns. Cached mail is user-scoped device storage; logging out clears bodies and headers but keeps queued Outbox messages.

## Updates

The updater checks:

```text
<server-url>/android/latest.json
```

Expected metadata:

```json
{
  "versionCode": 2,
  "versionName": "0.2.0",
  "apkUrl": "https://mail.example.com/android/rolltop.apk",
  "sha256": "optional lowercase hex sha256"
}
```

The app checks lightweight metadata whenever it enters the foreground and also uses persistent, network-constrained WorkManager scheduling. When `versionCode` is newer than the installed app, the APK is downloaded, optionally verified with `sha256`, and offered through an in-app dialog and update notification. A validated download is reused for the same version and remains available across app restarts, including when notification permission is denied.

Android does not allow a normal sideloaded app to silently replace itself. Users must confirm the install prompt, and may need to allow Rolltop to install unknown apps.

Update APKs must always use the Rolltop application ID, the offered version code, and a signing certificate compatible with the installed app. The client verifies all three before showing an install action. CI publishes `/android/latest.json` and `/android/rolltop.apk` into the server image only when all five repository secrets are configured:

```text
ROLLTOP_ANDROID_KEYSTORE_BASE64
ROLLTOP_ANDROID_STORE_PASSWORD
ROLLTOP_ANDROID_KEY_ALIAS
ROLLTOP_ANDROID_KEY_PASSWORD
ROLLTOP_ANDROID_CERT_SHA256
```

`ROLLTOP_ANDROID_KEYSTORE_BASE64` is the base64 encoding of the release keystore. `ROLLTOP_ANDROID_CERT_SHA256` is the release certificate's SHA-256 digest as printed by `keytool -list -v` for the keystore alias, or by `apksigner verify --print-certs` for a locally signed APK; CI pins it to prevent an accidental keystore change. Keep the original keystore backed up outside GitHub; losing it prevents updates to existing installs. Pull requests and untagged builds without these secrets still produce a debug APK artifact, but it is deliberately not published as a server update. Tagged builds fail when release signing is unavailable or only partially configured.

CI assigns a monotonic version code from `GITHUB_RUN_NUMBER`, verifies the APK signature, and reads the resulting APK metadata when writing `latest.json`, so the feed and APK cannot disagree. It copies the verified release APK and feed into `frontend/public/android` before Vite builds the production frontend; the Docker build therefore receives those exact signed files in `frontend/dist/android`. CI then extracts the files from the built image and compares their bytes, checksum, and APK version before publishing the image. Signing secrets and the keystore never enter the Docker build context. Android unit tests exercise the newer-version offer, same-origin APK, checksum, notification action, and destination-binding policies. Local builds default to version code 2; `ROLLTOP_ANDROID_VERSION_CODE` and `ROLLTOP_ANDROID_VERSION_NAME` can override it.

## Testing Server Updates

1. Configure the five signing secrets and deploy a CI-built server image.
2. Install its release APK. An app previously installed from the legacy debug build must be uninstalled once because Android cannot replace an APK signed by a different certificate.
3. Run and deploy a later CI build. Its version code will be higher.
4. Fully close and reopen Rolltop. It should download the same-origin APK and offer **Install**. Accept the one-time "install unknown apps" permission if Android requests it, then return to the update prompt or notification.

After that signing transition, later server APKs install in place and preserve WebView sessions and preferences.
