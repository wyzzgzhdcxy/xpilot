// 把原版 pac 里实际生效的域名清单抽出来，生成简洁版 pac。
//
// 用法：node tools/pac_rewrite.js
// 输入：D:\code\xpilot\html\pac.src  （转换工具产出的原始 gfwlist 版，需自行放好）
// 输出：D:\code\xpilot\html\pac      （覆盖）
//
// 只做一件事：把 `rules[0][1]`（走 PROXY 的那一组）平铺成单层数组，
// 逻辑简化为「命中即代理，否则直连」。域名一个字都不改、不排序、不去重
// （去重会改变条目数，而等价性验证要能逐条对上）。

const fs = require('fs');
const path = require('path');

const SRC = path.resolve(__dirname, '..', 'html', 'pac.src');
const DST = path.resolve(__dirname, '..', 'html', 'pac');

const src = fs.readFileSync(SRC, 'utf8');
const m = src.match(/var rules\s*=\s*([\s\S]*?)\n\];/);
if (!m) throw new Error('rules 未找到');
const rules = eval('(' + m[1] + '\n])');

// 收集所有走向 PROXY 的域名（子块下标为奇数 = 奇偶交替里的 PROXY 一侧）
const proxyDomains = [];
rules.forEach(blk => {
  blk.forEach((sub, subIdx) => {
    if (subIdx % 2 === 1) for (const d of sub) proxyDomains.push(d);
  });
});
// 再收集走向 DIRECT 的域名（偶数下标）—— 本文件里为空，但保留处理以防将来有
const directDomains = [];
rules.forEach(blk => {
  blk.forEach((sub, subIdx) => {
    if (subIdx % 2 === 0) for (const d of sub) directDomains.push(d);
  });
});

console.log('走代理的域名数:', proxyDomains.length);
console.log('走直连的域名数:', directDomains.length);

const header = `// xpilot PAC 规则 —— 域名白名单模式
//
// 判定口径（一句话）：host 命中列表里的任一条 → 走代理；否则直连。
// 匹配规则：host 与规则完全相等，或以 "." + 规则 结尾（覆盖子域名）。
//   带点是为了防后缀陷阱 —— "notgoogle.com" 不会被 "google.com" 误命中。
//
// 想加域名：直接往 proxyList 里加一行即可，改完在控制台点「刷新 PAC」。
// 注意不要用 Array.includes() 做匹配，它只比精确相等，会漏掉子域名。
`;

const body = [
  "var proxy = 'PROXY 127.0.0.1:10808;DIRECT;';",
  '',
  'var proxyList = [',
  ...proxyDomains.map((d, i) =>
    '    ' + JSON.stringify(d) + (i === proxyDomains.length - 1 ? '' : ',')),
  '];',
  '',
];

// 有 directDomains 时才生成 directList（当前为空，跳过以保持输出干净）
const directBlock = directDomains.length
  ? [
      '// 明确不走代理的域名（优先于 proxyList 判断前先检查）',
      'var directList = [',
      ...directDomains.map((d, i) =>
        '    ' + JSON.stringify(d) + (i === directDomains.length - 1 ? '' : ',')),
      '];',
      '',
    ]
  : [];

const logic = directDomains.length
  ? [
      'function FindProxyForURL(url, host) {',
      '    host = host.toLowerCase();',
      '    if (matchAny(host, directList)) return "DIRECT";',
      '    if (matchAny(host, proxyList)) return proxy;',
      '    return "DIRECT";',
      '}',
    ]
  : [
      'function FindProxyForURL(url, host) {',
      '    host = host.toLowerCase();',
      '    return matchAny(host, proxyList) ? proxy : "DIRECT";',
      '}',
    ];

const matcher = [
  '',
  '// matchAny 判断 host 是否命中列表中的任一条。',
  '// 只比较精确相等与「点号后缀」，与区分大小写无关（host 已转小写）。',
  'function matchAny(host, list) {',
  '    for (var i = 0; i < list.length; i++) {',
  '        var rule = list[i];',
  '        if (host === rule || host.endsWith("." + rule)) return true;',
  '    }',
  '    return false;',
  '}',
  '',
];

const out = header + '\n' + body.join('\n') + directBlock.join('\n') + logic.join('\n') + matcher.join('\n');
fs.writeFileSync(DST, out, 'utf8');
console.log('已写出:', DST);
console.log('新文件行数:', out.split('\n').length);
