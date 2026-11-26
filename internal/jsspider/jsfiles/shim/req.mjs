// 给 drpy2 引擎用的宿主 req()：引擎要求同步返回，所以用 curl 子进程同步实现。
import { spawnSync } from 'child_process';
import fs from 'fs'; import os from 'os'; import path from 'path'; import net from 'net';
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'jsreq-'));
const jar = path.join(tmp, 'jar'); let n = 0;

// 尽力而为的 SSRF 防护：第三方站源脚本不许访问内网/本机。
function isPrivate(ip) {
  if (net.isIPv4(ip)) {
    const [a, b] = ip.split('.').map(Number);
    return a === 10 || a === 127 || a === 0 || (a === 169 && b === 254) || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || a >= 224;
  }
  if (net.isIPv6(ip)) { const l = ip.toLowerCase(); return l === '::1' || l === '::' || l.startsWith('fe80') || l.startsWith('fc') || l.startsWith('fd'); }
  return false;
}
const dnsCache = new Map();
function hostAllowed(host) {
  if (net.isIP(host)) return !isPrivate(host);
  if (host === 'localhost' || host.endsWith('.local') || host.endsWith('.internal')) return false;
  if (dnsCache.has(host)) return dnsCache.get(host);
  let ok = true;
  const r = spawnSync('getent', ['ahosts', host], { encoding: 'utf8', timeout: 8000 });
  if (r.status === 0 && r.stdout) {
    for (const line of r.stdout.split('\n')) { const ip = line.split(/\s+/)[0]; if (ip && isPrivate(ip)) { ok = false; break; } }
  }
  dnsCache.set(host, ok); return ok;
}

function sniff(buf, ct) {
  let m = /charset=["']?([\w-]+)/i.exec(ct || ''); if (m) return m[1];
  const head = buf.subarray(0, 3000).toString('latin1');
  m = /<meta[^>]+charset=["']?([\w-]+)/i.exec(head) || /<meta[^>]+content=["'][^"']*charset=([\w-]+)/i.exec(head);
  return m ? m[1] : 'utf-8';
}

const RELAY = process.env.SPIDER_RELAY || '';
const relayHosts = new Map();   // 直连失败过的主机 -> 改走跳板的截止时间（10 分钟）
function viaRelay(url, method, H, body) {
  try {
    const pf = path.join(tmp, 'rp' + (++n));
    fs.writeFileSync(pf, JSON.stringify({ url, method, headers: H, body: body === undefined ? '' : Buffer.from(typeof body === 'string' ? body : JSON.stringify(body)).toString('base64') }));
    const r = spawnSync('curl', ['-s', '--max-time', '45', '-X', 'POST', '-H', 'Content-Type: application/json', '--data-binary', '@' + pf, RELAY], { maxBuffer: 50e6 });
    try { fs.unlinkSync(pf); } catch {}
    const d = JSON.parse(r.stdout.toString('utf8'));
    if (d.err || !d.status) return null;
    const headers = {}; for (const [k, v] of Object.entries(d.headers || {})) headers[k.toLowerCase()] = v;
    return { status: d.status, headers, body: Buffer.from(d.body || '', 'base64') };
  } catch { return null; }
}

globalThis.req = (url, o = {}) => {
  let host = '';
  try { host = new URL(url).hostname.replace(/^\[|\]$/g, ''); } catch { return { content: '', headers: {} }; }
  if (!hostAllowed(host)) return { content: '', headers: {} };
  const id = ++n, hf = path.join(tmp, 'h' + id), bf = path.join(tmp, 'b' + id);
  const a = ['-s', '-k', '--compressed', '-D', hf, '-o', bf, '-b', jar, '-c', jar, '--max-time', String(Math.max(5, Math.min(30, (o.timeout || 15000) / 1000)))];
  if (o.redirect !== 0 && o.redirect !== false) a.push('-L', '--max-redirs', '8', '--proto-redir', '=http,https');
  a.push('--proto', '=http,https');
  const H = o.headers || {};
  for (const [k, v] of Object.entries(H)) a.push('-H', `${k}: ${v}`);
  const ct = Object.entries(H).find(([k]) => k.toLowerCase() === 'content-type')?.[1] || '';
  const method = (o.method || 'GET').toUpperCase();
  let body = o.body;
  if (body === undefined && o.data !== undefined) body = /json/i.test(ct) ? JSON.stringify(o.data) : (typeof o.data === 'string' ? o.data : new URLSearchParams(o.data).toString());
  if (method === 'POST' || body) {
    a.push('-X', method === 'GET' ? 'POST' : method);
    if (body !== undefined) { a.push('--data-binary', typeof body === 'string' ? body : JSON.stringify(body)); if (!ct) a.push('-H', 'Content-Type: application/x-www-form-urlencoded'); }
  }
  a.push(url);
  if (RELAY && (relayHosts.get(host) || 0) > Date.now()) {
    const rr0 = viaRelay(url, method, H, body);
    if (rr0) {
      let c0;
      if (o.buffer === 2) c0 = rr0.body.toString('base64');
      else { let cs = sniff(rr0.body, rr0.headers['content-type']).toLowerCase(); if (cs === 'gb2312') cs = 'gbk'; try { c0 = new TextDecoder(cs).decode(rr0.body); } catch { c0 = rr0.body.toString('utf8'); } }
      return { content: c0, headers: rr0.headers };
    }
  }
  const cr = spawnSync('curl', a, { maxBuffer: 50e6 });
  let buf = Buffer.alloc(0); let headers = {}; let code = 0;
  try { buf = fs.readFileSync(bf); } catch {}
  try {
    const blocks = fs.readFileSync(hf, 'latin1').split(/\r?\n\r?\n/).filter(x => x.trim());
    const last = blocks[blocks.length - 1] || '';
    code = parseInt((last.split(/\r?\n/)[0] || '').split(' ')[1]) || 0;
    last.split(/\r?\n/).slice(1).forEach(l => { const j = l.indexOf(':'); if (j > 0) { const k = l.slice(0, j).toLowerCase(), v = l.slice(j + 1).trim(); headers[k] = headers[k] ? headers[k] + ';' + v : v; } });
  } catch {}
  try { fs.unlinkSync(hf); fs.unlinkSync(bf); } catch {}
  // 地区封锁兜底：超时/被重置/TLS 失败（curl 退出码非 0，且不是 DNS 失败 6、拒绝连接 7）或 403/451 时，
  // 让 Go 端通过 SSH_CMDS_GEO_BLOCK 跳板重试一次。
  const relayWorthy = (cr.status !== 0 && cr.status !== 6 && cr.status !== 7) || code === 403 || code === 451;
  if (RELAY && relayWorthy) {
    const rr = viaRelay(url, method, H, body);
    const origOk = cr.status === 0 && code > 0 && code < 400;
    if (rr && (rr.status < 400 || !origOk)) { buf = rr.body; headers = rr.headers; if (!origOk) relayHosts.set(host, Date.now() + 600000); }
  }
  let content;
  if (o.buffer === 2) content = buf.toString('base64');
  else {
    let cs = sniff(buf, headers['content-type']).toLowerCase(); if (cs === 'gb2312') cs = 'gbk';
    try { content = new TextDecoder(cs).decode(buf); } catch { content = buf.toString('utf8'); }
  }
  return { content, headers };
};
const store = {};
globalThis.local = { get: (r, k) => store[r + '$' + k], set: (r, k, v) => { store[r + '$' + k] = v; }, delete: (r, k) => { delete store[r + '$' + k]; } };
globalThis.getProxy = () => 'http://127.0.0.1:9978/proxy?do=js';
globalThis.window = globalThis;
