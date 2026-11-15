// sw.js —— xpilot 控制台的 Service Worker。
//
// 目标：让控制台可以"装到桌面"（PWA），并在后端进程没起来时仍能打开外壳页面。
//
// 缓存策略（刻意做得保守，因为控制台是实时看板）：
//   - 应用外壳（'/'）：网络优先，失败时回落到缓存 —— 离线也能开出一个页面，
//     页面随后会因为拉不到 /api/config 而点亮"连不上后端"横幅（这是正确行为）。
//   - 图标 / manifest：缓存优先 + 后台更新 —— 这些是静态资源，变了也不影响数据。
//   - /api/*（含 SSE 长连接）：**完全不缓存**，直接走网络。
//   - /pac：用户可编辑的文件，随时可能变，同样不缓存。
//   - 其余路径（运行报告等动态文件）：不缓存。
//
// 为什么只缓存"白名单里的几个静态文件"而不是整站 cache-first：
// 缓存整站会让"运行报告"显示上一次的旧数据，而旧数据比报错更难排查。

// ⚠️ 改动任何预缓存资源（图标、manifest、控制台外壳）时，把下面的版本号 +1：
// 缓存名变了，install 阶段会重新拉一遍全部预缓存资源，浏览器里的旧图标才会被替换。
// 只改图标、不动 sw.js 字节的话，浏览器根本不会重新安装 SW —— 图标会一直是旧的。
const CACHE = 'xpilot-v1';

// 需要离线可用的静态资源。'/' 是应用外壳，其余是图标与 manifest。
const PRECACHE = [
  '/',
  '/manifest.webmanifest',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/icon-maskable-512.png',
];

// 只对这些路径启用缓存优先；其余一律不碰缓存。
const CACHEABLE = new Set(PRECACHE);

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE)
      // addAll 是全有或全无：任何一个资源拉不到，整个安装就失败。
      // 图标是内嵌资源，正常不会失败；真失败了也不该让 SW 装不上。
      .then((c) => c.addAll(PRECACHE).catch(() => {}))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// 页面主动要求立刻接管（更新后无需等所有标签页关闭）
self.addEventListener('message', (e) => {
  if (e.data === 'skipWaiting') self.skipWaiting();
});

self.addEventListener('fetch', (e) => {
  const req = e.request;

  // 只处理 GET：控制台的写操作全是 POST，绝不能被缓存或重放。
  if (req.method !== 'GET') return;

  let url;
  try {
    url = new URL(req.url);
  } catch (_) {
    return;
  }
  if (url.origin !== self.location.origin) return;

  // 实时数据通道一律直连网络：
  //   /api/events、/api/access-log-stream 是 SSE 长连接，缓存会把它挂死；
  //   其它 /api/* 是选点进度与参数快照，必须是最新的。
  if (url.pathname.startsWith('/api/')) return;

  // PAC 是用户自己维护的文件（改完点"刷新 PAC"就生效），不能缓存。
  if (url.pathname === '/pac') return;

  // 导航请求：网络优先，失败回落到缓存里的应用外壳。
  // 只缓存 '/' 这一个外壳；运行报告之类的动态页面不在此列。
  if (req.mode === 'navigate') {
    e.respondWith(
      fetch(req)
        .then((res) => {
          if (res && res.ok && url.pathname === '/') {
            const copy = res.clone();
            caches.open(CACHE).then((c) => c.put('/', copy)).catch(() => {});
          }
          return res;
        })
        .catch(() => {
          if (url.pathname === '/') {
            return caches.match('/').then((hit) => hit || Response.error());
          }
          return Response.error();
        })
    );
    return;
  }

  // 静态资源：缓存优先，命中后在后台悄悄更新一份。
  if (CACHEABLE.has(url.pathname)) {
    e.respondWith(
      caches.match(req).then((hit) => {
        const refresh = fetch(req)
          .then((res) => {
            if (res && res.ok) {
              const copy = res.clone();
              caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
            }
            return res;
          })
          .catch(() => hit);
        return hit || refresh;
      })
    );
  }
});
