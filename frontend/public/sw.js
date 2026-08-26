const STATIC_CACHE = "rolltop-static-v10";
const STATIC_ASSETS = ["/offline.html", "/manifest.webmanifest", "/icon.svg", "/icon.svg?v=transparent-logo-v2"];
// The neutral shell is the app document without embedded session data (the
// server only serves it for ?shell=1). It lets a cold app start boot the SPA
// into cached mail instead of the offline page.
const SHELL_URL = "/mail?shell=1";
const SHELL_MAX_AGE_MS = 20 * 60 * 60 * 1000;
let securityUnlockUserID = 0;
let securityUnlockState = { unlockedUntil: 0, keys: [] };
let securityUnlockTimer = 0;

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(STATIC_CACHE).then((cache) => cache.addAll(STATIC_ASSETS)));
  event.waitUntil(primeShell());
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((key) => key !== STATIC_CACHE).map((key) => caches.delete(key)))
    )
  );
  self.clients.claim();
});

async function primeShell() {
  try {
    const res = await fetch(SHELL_URL, { credentials: "omit", cache: "no-store" });
    if (!res.ok) return;
    const text = await res.text();
    // Defense in depth: never store a document that carries session data.
    // The inert <meta> marker is expected; the injected script is not.
    if (text.includes('<script id="rolltop-startup"')) return;
    const headers = new Headers({ "Content-Type": "text/html; charset=utf-8", "X-Rolltop-Shell-Stored": String(Date.now()) });
    const cache = await caches.open(STATIC_CACHE);
    await cache.put(SHELL_URL, new Response(text, { status: 200, headers }));
  } catch {
    // Priming while offline is fine; an older cached shell still works.
  }
}

function shellIsStale(res) {
  if (!res) return true;
  const storedAt = Number(res.headers.get("X-Rolltop-Shell-Stored") || 0);
  return !storedAt || Date.now() - storedAt > SHELL_MAX_AGE_MS;
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== self.location.origin) return;

  if (url.pathname.startsWith("/rolltop-native-share/")) {
    event.respondWith(fetch(req));
    return;
  }

  if (url.pathname.startsWith("/brand-icons/") || url.pathname.startsWith("/plugins/")) return;

  if (url.pathname.startsWith("/api/")) return;

  const acceptsHTML = req.headers.get("Accept")?.includes("text/html");
  if (req.mode === "navigate" || acceptsHTML) {
    const networkRequest = new Request(req, { cache: "no-cache" });
    event.respondWith(
      fetch(networkRequest)
        .then((res) => {
          if (req.mode === "navigate") {
            event.waitUntil(
              caches.match(SHELL_URL).then((cached) => {
                if (shellIsStale(cached)) return primeShell();
              })
            );
          }
          return res;
        })
        .catch(() => {
          if (req.mode !== "navigate") return Promise.reject(new Error("offline"));
          // Offline cold start: boot the neutral shell so the app can render
          // cached mail; fall back to the offline page before it was ever primed.
          return caches.match(SHELL_URL).then((shell) => shell || caches.match("/offline.html"));
        })
    );
    return;
  }

  // Never put authenticated mail, attachment, blob, or image responses in a
  // URL-only cache; user-local IDs can overlap after a session change.
  const cacheablePublicAsset = url.pathname.startsWith("/assets/") || STATIC_ASSETS.includes(`${url.pathname}${url.search}`) || STATIC_ASSETS.includes(url.pathname);
  if (!cacheablePublicAsset) return;

  event.respondWith(
    fetch(req)
      .then((res) => {
        if (res.ok) caches.open(STATIC_CACHE).then((cache) => cache.put(req, res.clone()));
        return res;
      })
      .catch(() => caches.match(req))
  );
});

self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    data = {};
  }
  const title = typeof data.title === "string" && data.title ? data.title : "rolltop";
  const icon = typeof data.icon === "string" && data.icon ? data.icon : "/icon.svg?v=transparent-logo-v2";
  const options = {
    body: typeof data.body === "string" ? data.body : "New mail synced.",
    tag: typeof data.tag === "string" && data.tag ? data.tag : "rolltop-new-mail",
    icon,
    badge: typeof data.badge === "string" && data.badge ? data.badge : icon,
    data: {
      url: sameOriginPath(data.url, "/mail"),
      apiURL: sameOriginPath(data.api_url, ""),
      messageID: Number(data.message_id || 0)
    }
  };
  event.waitUntil(Promise.all([
    warmNotificationMessage(options.data),
    self.registration.showNotification(title, options)
  ]));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const data = event.notification.data || {};
  const targetURL = new URL(sameOriginPath(data.url, "/mail"), self.location.origin).href;
  event.waitUntil(
    Promise.all([
      warmNotificationMessage(data),
      self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(async (clients) => {
        for (const client of clients) {
          if (!client.url.startsWith(self.location.origin)) continue;
          if ("navigate" in client && client.url !== targetURL) {
            const navigated = await client.navigate(targetURL);
            return navigated?.focus();
          }
          return client.focus();
        }
        return self.clients.openWindow(targetURL);
      })
    ])
  );
});

function sameOriginPath(value, fallback) {
  if (typeof value !== "string" || !value) return fallback;
  try {
    const url = new URL(value, self.location.origin);
    if (url.origin !== self.location.origin) return fallback;
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return fallback;
  }
}

async function warmNotificationMessage(data) {
  const apiURL = sameOriginPath(data?.apiURL, "");
  if (!apiURL) return;
  const url = new URL(apiURL, self.location.origin);
  if (!isNotificationWarmAPIURL(url)) return;
  try {
    await fetch(apiURL, {
      method: "GET",
      headers: { Accept: "application/json" },
      credentials: "include",
      cache: "reload"
    });
  } catch {
    // Navigation should still work if the background warm-up misses.
  }
}

function isNotificationWarmAPIURL(url) {
  if (/^\/api\/messages\/\d+\/prefetch$/.test(url.pathname) && !url.search) return true;
  return url.pathname === "/api/mail" && url.searchParams.get("page") === "1" && Array.from(url.searchParams.keys()).length === 1;
}

self.addEventListener("message", (event) => {
  const data = event.data || {};
  if (data.type === "rolltop:refresh-shell") {
    event.waitUntil(primeShell());
    return;
  }
  if (data.type === "rolltop:security-unlock-get") {
    const userID = Number(data.userID || 0);
    const state = currentSecurityUnlockState(userID);
    if (!state.unlockedUntil) requestSecurityUnlockStateFromClients(userID);
    event.source?.postMessage({ type: "rolltop:security-unlock-state", userID, state });
    return;
  }
  if (data.type !== "rolltop:security-unlock-set") return;
  const userID = Number(data.userID || 0);
  const state = normalizeSecurityUnlockState(data.state);
  securityUnlockUserID = userID;
  securityUnlockState = state;
  scheduleSecurityLock();
  broadcastSecurityUnlockState(userID);
});

function normalizeSecurityUnlockState(state) {
  const unlockedUntil = Number(state?.unlockedUntil || 0);
  if (!unlockedUntil || unlockedUntil <= Date.now()) return { unlockedUntil: 0, keys: [] };
  const keys = Array.isArray(state?.keys) ? state.keys.filter((key) => key && key.private_key_armored) : [];
  return keys.length > 0 ? { unlockedUntil, keys } : { unlockedUntil: 0, keys: [] };
}

function currentSecurityUnlockState(userID) {
  if (userID !== securityUnlockUserID) return { unlockedUntil: 0, keys: [] };
  securityUnlockState = normalizeSecurityUnlockState(securityUnlockState);
  if (!securityUnlockState.unlockedUntil) securityUnlockUserID = 0;
  return securityUnlockState;
}

function scheduleSecurityLock() {
  if (securityUnlockTimer) clearTimeout(securityUnlockTimer);
  securityUnlockTimer = 0;
  if (!securityUnlockState.unlockedUntil) return;
  securityUnlockTimer = setTimeout(() => {
    const userID = securityUnlockUserID;
    securityUnlockUserID = 0;
    securityUnlockState = { unlockedUntil: 0, keys: [] };
    broadcastSecurityUnlockState(userID);
  }, Math.max(0, securityUnlockState.unlockedUntil - Date.now()));
}

async function broadcastSecurityUnlockState(userID) {
  const clients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const state = currentSecurityUnlockState(userID);
  clients.forEach((client) => client.postMessage({ type: "rolltop:security-unlock-state", userID, state }));
}

async function requestSecurityUnlockStateFromClients(userID) {
  const clients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  clients.forEach((client) => client.postMessage({ type: "rolltop:security-unlock-request", userID }));
}
