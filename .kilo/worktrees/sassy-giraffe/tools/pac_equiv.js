// pac 重写等价性验证：把改写前的判定逻辑与改写后的逐条对比。
//
// 用法：node tools/pac_equiv.js
// 退出码 0 = 等价；1 = 存在真正的逻辑差异（会打印差异明细）。
//
// 需要 html/pac.src（转换工具产出的原始版）存在；它由 pac_rewrite.js 的输入提供。
//
// 为什么值得写这个：pac 的走向决定「流量进不进代理」，
// 判错了会静默直连（或静默走代理），用户很难察觉。靠肉眼看 20 行 vs 8 行不够，
// 必须让机器把 6788 条域名 + 边界用例全跑一遍对比。

const fs = require('fs');
const path = require('path');

const ORIG = path.resolve(__dirname, '..', 'html', 'pac.src');
const NEW = path.resolve(__dirname, '..', 'html', 'pac');

// ---- 从两份文件里各自取出一套判定函数 ----
//
// 两边都**执行真实文件**，不手抄逻辑。
// 这里踩过两次坑，都是「手抄导致验证自证」：
//   ① 手抄的新版 decide() 漏了 toLowerCase()，于是报告「0 差异」而实际已分叉；
//   ② 调函数时只传 host，漏了 url 参数，host 全程是 undefined，满屏 THROW。
// 结论：验证「A 与 B 行为一致」时，两边都必须跑真文件、且按真实签名调用。
function loadRealFile(path, needMarker) {
  const src = fs.readFileSync(path, 'utf8');
  if (!src.includes(needMarker)) throw new Error('文件缺少标识 ' + needMarker + ': ' + path);
  const fn = new Function(
    src + '\nreturn { decide: FindProxyForURL,' +
    ' list: (typeof proxyList !== "undefined" ? proxyList : null),' +
    ' rules: (typeof rules !== "undefined" ? rules : null) };'
  );
  const api = fn();
  if (typeof api.decide !== 'function') throw new Error('FindProxyForURL 未导出: ' + path);
  return api;
}

function loadOriginal(path) {
  const src = fs.readFileSync(path, 'utf8');
  const api = loadRealFile(path, 'var rules');
  // 顺带把原版实际生效的域名清单抽出来（供重写脚本使用）
  const rules = api.rules;
  const domains = [];
  rules.forEach((blk, blockIdx) => {
    blk.forEach((sub, subIdx) => {
      for (const d of sub) domains.push({ d, block: blockIdx, sub: subIdx });
    });
  });
  return { decide: api.decide, domains, rules };
}

// 新版：**直接执行 pac 文件本身**，不手抄逻辑。
//
// 这里踩过一个坑：早先版本我手抄了一份 decide() 做对比，结果漏抄了
// toLowerCase()，导致验证报告「0 差异」而实际行为已经分叉。
// 教训 —— 验证「文件 A 与文件 B 行为一致」时，两边都必须执行真实文件，
// 任何一方手抄都可能抄漏，让验证变成自我确认。
function loadNew(path) {
  const api = loadRealFile(path, 'var proxyList');
  return { decide: api.decide, proxyList: api.list || [] };
}

const orig = loadOriginal(ORIG);
const next = loadNew(NEW);

console.log('原版域名条目总数:', orig.domains.length);
console.log('新版 proxyList 条目总数:', next.proxyList.length);
console.log();

// ---- 构造测试集 ----
const cases = [];

// 1) 原版里每一条域名本身
for (const { d } of orig.domains) cases.push(d);

// 2) 每条域名的子域形式（验证后缀匹配）
for (const { d } of orig.domains) cases.push('www.' + d, 'a.b.' + d);

// 3) 大小写变体 —— 这一维必须覆盖够
//    原版依赖「浏览器传入的 host 已是小写」这一规范保证，自己不做转换；
//    新版主动 toLowerCase()。两者在规范内行为一致，一旦输入非小写就会分叉。
//    测试集里只放一两个大写样本是不够的，会掩盖差异，所以对全部域名都造变体。
for (const { d } of orig.domains) {
  cases.push(d.toUpperCase());
  if (d.length > 3) cases.push(d.slice(0, 1).toUpperCase() + d.slice(1)); // 首字母大写
}

// 4) 边界与陷阱用例
cases.push(
  // 后缀陷阱：不应被父域误伤
  'notgoogle.com', 'xgithub.com', 'fake-taobao.com',
  // 未列举的普通域名 → 应直连
  'taobao.com', 'qq.com', 'baidu.com', 'some-random-xyz-12345.com',
  // 精确等于规则本身
  'google.com', 'github.com',
  // 空串与奇怪输入
  '', 'a', '.', 'com',
  // 只有父域后缀相同但非边界
  'mygithub.com',
);

// ---- 调用约定 ----
// PAC 规范签名是 FindProxyForURL(url, host)，**两个参数**。
// 这里踩过坑：早先只传 host，导致 host 实际是 undefined 而全程抛错，
// 验证结果全是 THROW，看着像「新版坏了」，其实是我调错了。
// 两版都按真实签名调用，并用同一个假 url 保持公平。
function call(decide, host) {
  try {
    return decide('https://' + host + '/', host);
  } catch (e) {
    return 'THROW:' + e.message;
  }
}

// ---- 逐条对比 ----
let diff = 0;
let caseOnlyDiff = 0;
const diffs = [];
const seen = new Set();
for (const host of cases) {
  if (seen.has(host)) continue;
  seen.add(host);
  const a = call(orig.decide, host);
  const b = call(next.decide, host);
  if (a !== b) {
    // 区分「大小写差异」与「真正的逻辑差异」：
    // 把输入转小写后两版一致 → 差异只源于大小写处理，属有意改进；
    // 转小写后仍不一致 → 真的逻辑差异，必须修。
    const lower = String(host).toLowerCase();
    const sameAfterLower = call(orig.decide, lower) === call(next.decide, lower);
    if (host !== lower && sameAfterLower) {
      caseOnlyDiff++;
    } else {
      diff++;
      if (diffs.length < 30) diffs.push({ host, orig: a, new: b });
    }
  }
}

console.log('对比用例数(去重后):', seen.size);
console.log('真正的逻辑差异数:', diff, ' ← 必须为 0');
console.log('仅大小写导致的差异数:', caseOnlyDiff, ' ← 有意改进，见下方说明');
console.log();

if (diff) {
  console.log('逻辑差异明细（最多 30 条）:');
  for (const d of diffs) {
    console.log(`  ${d.host.padEnd(60)} 原=${d.orig}  新=${d.new}`);
  }
  process.exit(1);
}

if (caseOnlyDiff) {
  console.log('大小写差异说明：');
  console.log('  原版不做 toLowerCase，依赖 PAC 规范「host 由浏览器转小写」的保证；');
  console.log('  新版主动转换，因此对非小写输入会判定为 PROXY（原版会静默直连）。');
  console.log('  这是有意的健壮性改进，不是回归 —— 规范内行为两者一致。');
  console.log();
}

// ---- 额外：确认原版里「真正生效」的域名集合都被搬过去了 ----
// 原版只有 sub 为奇数的子块才是 PROXY；偶数子块是 DIRECT 组（这里是空的）
const proxyDomains = orig.domains.filter(x => x.sub % 2 === 1).map(x => x.d);
const missing = proxyDomains.filter(d => !next.proxyList.includes(d));
console.log('原版中走代理的域名数:', proxyDomains.length);
console.log('新版遗漏的域名数:', missing.length);
if (missing.length) {
  console.log('遗漏样例:', missing.slice(0, 10));
  process.exit(1);
}

// 反向：新版里有没有多出来的
const extra = next.proxyList.filter(d => !proxyDomains.includes(d));
console.log('新版多出的域名数:', extra.length);
if (extra.length) {
  console.log('多出样例:', extra.slice(0, 10));
  process.exit(1);
}

// ---- 抽样打印，人工过目 ----
// 注意也走 call()（两参调用），别在这里退回一参写法。
console.log();
console.log('抽样决策（原版 vs 新版）:');
console.log('-'.repeat(72));
for (const h of ['chatgpt.com', 'api.github.com', 'taobao.com', 'qq.com',
                 'notgoogle.com', 'some-random-xyz-12345.com', 'www.google.com',
                 'GOOGLE.COM']) {
  const a = call(orig.decide, h) === 'DIRECT' ? 'DIRECT' : 'PROXY';
  const b = call(next.decide, h) === 'DIRECT' ? 'DIRECT' : 'PROXY';
  console.log(`  ${h.padEnd(30)} 原=${a.padEnd(7)} 新=${b.padEnd(7)} ${a === b ? 'ok' : '有差异(大小写)'}`);
}

console.log();
console.log('结论：');
console.log('  · 逻辑差异 0 —— 规范内行为与原版完全一致；');
console.log('  · 大小写差异 ' + caseOnlyDiff + ' 条 —— 新版主动转小写，是有意的健壮性改进。');
console.log('等价性验证通过。');
