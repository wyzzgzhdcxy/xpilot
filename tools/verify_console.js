// verify_console.js —— 用 Edge CDP 验证控制台页面的关键行为（零依赖，Node 22+ 自带 WebSocket）。
//
// 用法：
//   1) 先起一个带调试端口的 Edge：
//      msedge --headless=new --remote-debugging-port=9222 --user-data-dir=<临时目录> about:blank
//   2) node tools/verify_console.js
//
// ★ 坑：pacDomains 是 let 声明，不挂 window；CDP evaluate 与页面同作用域，直接按名引用。
// ★ 坑：无头模式 rAF 被节流到 ~30fps，不要用 rAF 量耗时（详见 bench_pac_frame.js）。
const PAGE = process.env.XPILOT_PAGE || 'http://127.0.0.1:8888/';

async function cdp() {
  const list = await (await fetch('http://127.0.0.1:9222/json/list')).json();
  let target = list.find(t => t.type === 'page');
  if (!target) throw new Error('没有可用的 page target');
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0;
  const pend = new Map();
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); }
  };
  const send = (method, params) => new Promise(res => {
    const n = ++id; pend.set(n, res);
    ws.send(JSON.stringify({ id: n, method, params: params || {} }));
  });
  const evalIn = async (expr) => {
    const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails));
    return r.result && r.result.result ? r.result.result.value : undefined;
  };
  return { send, evalIn, close: () => ws.close() };
}

(async () => {
  const c = await cdp();
  await c.send('Page.enable');
  await c.send('Page.navigate', { url: PAGE });
  await new Promise(r => setTimeout(r, 2500));

  // ★ 坑：非当前标签的 .tabpage 是 display:none，其内部元素 getBoundingClientRect()
  //   全返回 0。所以量 PAC 卡片布局前必须先 showTab 切过去（并等一次文件加载）。
  //   showTab 的签名是 (id, btn)，btn 会加 .active 类，所以要传对应的 .tabbtn 元素。
  const nav = async (tabId, waitMs) => {
    await c.evalIn(`(() => {
      const btn = document.querySelector('.tabbtn[onclick*="${tabId}"]')
               || document.querySelectorAll('.tabbtn')[0];
      if (typeof showTab === 'function') showTab('${tabId}', btn);
      return true;
    })()`);
    await new Promise(r => setTimeout(r, waitMs || 1500));
  };

  const out = {};

  // ---- A. 运行状态页：选点历史相关（默认页，无需切换）----
  Object.assign(out, await c.evalIn(`(() => {
    const rep = {};
    // 1) 「自动滚动到最新」勾选框是否已从 DOM 中彻底消失
    rep.histAutoCheckbox = !!document.getElementById('histAuto');
    rep.histAutoClassUsed = !!document.querySelector('.histauto');
    // 2) 页面上任何位置都不该再有「自动滚动」字样（防只删 DOM 漏删文案）
    rep.anyAutoScrollText = document.body.innerText.includes('自动滚动');
    // 3) 选点详情文案是否提到落盘路径与上限
    const hs = document.getElementById('histStat');
    rep.histStatText = hs ? hs.textContent.trim() : '(无 #histStat)';
    return rep;
  })()`));

  // ---- B. PAC 规则页：搜索框布局（必须切页，否则 rect 全 0）----
  await nav('tab-pac', 2500);
  Object.assign(out, await c.evalIn(`(() => {
    const rep = {};
    const ps = document.querySelector('.pacsearch');
    const hb = document.querySelector('#tab-pac .fhead b');
    if (ps) {
      const r = ps.getBoundingClientRect();
      rep.pacSearchWidth = Math.round(r.width);
      // 宽 200 是设计值；若被 input[type=text]{width:100%} 覆盖会飙到 900+
      rep.pacSearchOK = r.width > 150 && r.width < 260 && r.height > 10;
    } else rep.pacSearchMissing = true;
    if (hb) {
      const r = hb.getBoundingClientRect();
      rep.pacTitleText = hb.textContent.trim();
      rep.pacTitleHeight = Math.round(r.height);
      rep.pacTitleWidth = Math.round(r.width);
      // 单行标题高约 20px；被 flex 压成竖排会飙到 100+
      rep.pacTitleWrappedVertical = r.height > 40;
    }
    rep.pacCountText = (document.getElementById('pacCount')||{}).textContent || '';
    return rep;
  })()`));

  // ---- C. 访问日志页：200 行上限 ----
  await nav('tab-log', 2500);
  Object.assign(out, await c.evalIn(`(() => {
    const rep = {};
    rep.logStatText = (document.getElementById('logStat')||{}).textContent || '';
    // 表格行数应 <= 200
    const rows = document.querySelectorAll('#logBody tr, #logTbl tbody tr');
    rep.logRowCount = rows.length;
    rep.logRowsWithinLimit = rows.length <= 200;
    return rep;
  })()`));

  console.log(JSON.stringify(out, null, 2));
  c.close();
})().catch(e => { console.error('FAIL:', e.message); process.exit(1); });
