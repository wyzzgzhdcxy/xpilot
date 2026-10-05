// 在真实（非无头节流）环境下量渲染开销。
// 关键手法：赋值后立刻读 offsetHeight，会强制浏览器同步完成布局（forced reflow），
// 从而把「布局耗时」纳入测量范围 —— 这正是 word-break:break-all 代价所在。
//
// 用法（需先启动 xpilot 与带调试端口的 Edge）：
//   1. 启动 Edge（PowerShell）：
//      Start-Process "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe" `
//        -ArgumentList '--headless=new','--disable-gpu','--no-first-run',
//          '--remote-debugging-port=9222',"--user-data-dir=$env:TEMP\edge-prof",'about:blank'
//   2. node tools/bench_pac_frame.js
//   3. 测完关掉 Edge：Get-Process msedge | Stop-Process -Force
//
// ★ 踩过的坑：别用 requestAnimationFrame 量耗时 —— 无头模式 rAF 被节流到约 30fps，
//   量出来恒定 ~33ms，与真实渲染开销无关，会得出「提速 1.0 倍」的假结论。
//   必须用「赋值 + 读 offsetHeight 强制同步布局」才能量到真实 reflow 成本。
// ★ 另一个坑：pacDomains 是 let 声明，不会挂到 window 上；CDP 的 evaluate 与页面
//   同处一个全局作用域，直接按变量名引用即可（不要写 window.pacDomains）。
// ★ 测量有波动（同一份代码两次跑可差一倍），比较时看数量级，别把 1~2ms 的差当结论。
const http = require('http');

function getJSON(path) {
  return new Promise((res, rej) => {
    http.get({ host: '127.0.0.1', port: 9222, path }, r => {
      let d = ''; r.on('data', c => d += c); r.on('end', () => res(JSON.parse(d)));
    }).on('error', rej);
  });
}

async function main() {
  const targets = await getJSON('/json/list');
  const page = targets.find(t => t.type === 'page');
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  let id = 0; const pending = new Map();
  ws.addEventListener('message', ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  });
  await new Promise(r => ws.addEventListener('open', r));
  const send = (method, params = {}) => new Promise(res => {
    const myId = ++id; pending.set(myId, res);
    ws.send(JSON.stringify({ id: myId, method, params }));
  });
  const evalJs = async (expression) => {
    const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails));
    return r.result?.result?.value;
  };

  await send('Runtime.enable');
  await send('Page.enable');
  // 关掉 CPU 节流影响，并设为前台优先级
  await send('Emulation.setCPUThrottlingRate', { rate: 1 });
  await send('Page.navigate', { url: 'http://127.0.0.1:6001/' });
  await new Promise(r => setTimeout(r, 2500));
  await evalJs(`showTab('tab-pac', document.querySelectorAll('.tabbtn')[4])`);
  await new Promise(r => setTimeout(r, 1200));

  // 强制同步布局计入测量：赋值 → 读 offsetHeight
  const setup = await evalJs(`
    var __all = pacDomains;
    window.__bench = function(limit, iter) {
      var box = document.querySelector('#fsrc-pac');
      var all = __all;
      var total = 0, worst = 0;
      for (var k = 0; k < iter; k++) {
        var shown = (limit > 0 && all.length > limit) ? all.slice(0, limit) : all;
        var html = shown.map(function(d){
          return d.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
        }).join(',');
        var t0 = performance.now();
        box.innerHTML = html;
        void box.offsetHeight;       // 强制同步布局，把 reflow 计入
        var dt = performance.now() - t0;
        total += dt;
        if (dt > worst) worst = dt;
      }
      return { avg: total / iter, worst: worst, len: box.innerHTML.length, h: box.offsetHeight };
    };
    __all ? 'ready:' + __all.length : 'no-data';
  `);
  console.log('数据就绪:', setup);
  console.log('');
  console.log('=== 真实渲染耗时（赋值 + 强制同步布局）===');
  const cases = [['全量 6788 条', 0], ['限制 1000 条', 1000], ['限制 300 条', 300], ['限制 100 条', 100]];
  const results = {};
  for (const [label, limit] of cases) {
    const r = await evalJs(`window.__bench(${limit}, 15)`);
    results[limit] = r.avg;
    console.log(label.padEnd(16) + '平均 ' + r.avg.toFixed(2).padStart(6) + ' ms   最差 ' +
      r.worst.toFixed(2).padStart(6) + ' ms   内容 ' + String(r.len).padStart(6) +
      ' 字符   高 ' + r.h + 'px');
  }
  console.log('');
  console.log('=== 提速 ===');
  for (const n of [1000,300,100]) console.log('限制 '+n+' 条: ' + (results[0]/results[n]).toFixed(1) + ' 倍');

  await evalJs(`renderPacDomains('')`);
  ws.close();
  process.exit(0);
}

main().catch(e => { console.error('失败:', e.message); process.exit(1); });
