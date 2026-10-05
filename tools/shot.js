// shot.js —— 用 Edge CDP 给控制台各标签页截图（零依赖）。
// 用法: node tools/shot.js [输出目录]
// ★ 坑：非当前 .tabpage 是 display:none，截图前必须 showTab 切过去，否则拍到空白。
const PAGE = process.env.XPILOT_PAGE || 'http://127.0.0.1:6001/';
const OUT = process.argv[2] || 'C://Users//wangchaojun//AppData//Local//Temp//xpilot-shots';
const fs = require('fs');

async function cdp() {
  const list = await (await fetch('http://127.0.0.1:9222/json/list')).json();
  let t = list.find(x => x.type === 'page');
  if (!t) throw new Error('没有 page target');
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0; const pend = new Map();
  ws.onmessage = ev => { const m = JSON.parse(ev.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
  const send = (method, params) => new Promise(res => { const n = ++id; pend.set(n, res); ws.send(JSON.stringify({ id: n, method, params: params || {} })); });
  return { send, close: () => ws.close() };
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const c = await cdp();
  await c.send('Page.enable');
  await c.send('Emulation.setDeviceMetricsOverride', { width: 1280, height: 1400, deviceScaleFactor: 1, mobile: false });
  await c.send('Page.navigate', { url: PAGE });
  await new Promise(r => setTimeout(r, 3000));

  for (const [tab, name] of [['tab-status', 'status'], ['tab-pac', 'pac'], ['tab-log', 'log']]) {
    await c.send('Runtime.evaluate', { expression:
      `(() => { const b = document.querySelector('.tabbtn[onclick*="${tab}"]'); if (typeof showTab==='function') showTab('${tab}', b); return 1; })()` });
    await new Promise(r => setTimeout(r, 2500));
    const r = await c.send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
    const f = `${OUT}\\${name}.png`;
    fs.writeFileSync(f, Buffer.from(r.result.data, 'base64'));
    console.log('saved:', f);
  }
  c.close();
})().catch(e => { console.error('FAIL:', e.message); process.exit(1); });
