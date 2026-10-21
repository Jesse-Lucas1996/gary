// gary's service worker exists to make the dashboard installable and to show a
// readable page when the hub can't be reached. It never caches the dashboard,
// the API or the login page: those are live and behind a session, and a stale
// copy would show agents as working long after they stopped.
const CACHE = "gary-shell-v2";
const SHELL = ["icon-192.png", "icon-512.png"];

const OFFLINE = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>gary — offline</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0a0a0c;color:#e7e7ec;
font:15px/1.5 system-ui,sans-serif;text-align:center;padding:1rem}p{color:#8b8b98;margin:.3rem 0 1.2rem}
button{background:#7aa2f7;color:#0a0a0c;border:0;border-radius:8px;padding:.55rem 1.1rem;font:inherit;font-weight:600}</style>
</head><body><div><img src="icon-192.png" width="72" height="72" alt=""><h1 style="font-size:1.1rem;margin:.8rem 0 0">
Can't reach gary</h1><p>The hub isn't answering — check your connection or Tailscale.</p>
<button onclick="location.reload()">Try again</button></div></body></html>`;

self.addEventListener("install", e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", e => {
  e.waitUntil(caches.keys()
    .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener("fetch", e => {
  const req = e.request;
  if (req.method !== "GET") return;
  if (req.mode === "navigate") {
    e.respondWith(fetch(req).catch(() =>
      new Response(OFFLINE, {headers: {"Content-Type": "text/html; charset=utf-8"}})));
    return;
  }
  if (SHELL.some(p => new URL(req.url).pathname.endsWith("/" + p))) {
    e.respondWith(caches.match(req).then(hit => hit || fetch(req)));
  }
});

// A ping notification: tapping it brings the dashboard back instead of opening a
// second copy.
self.addEventListener("notificationclick", e => {
  e.notification.close();
  e.waitUntil(self.clients.matchAll({type: "window"}).then(list =>
    list.length ? list[0].focus() : self.clients.openWindow(self.registration.scope)));
});
