// JS（drpy2 规则）站源 worker：由 Go 端启动，stdin/stdout 一行一个 JSON。
//   请求 {"id":1,"op":"home|category|detail|search|play|proxy","site":"key","ext":"规则代码或地址","args":{...}}
//   响应 {"id":1,"ok":true,"data":...} / {"id":1,"ok":false,"err":"..."}
import './shim/req.mjs';
import './shim/jsoup.mjs';
import readline from 'readline';

const realLog = console.log;
console.log = () => {};   // 引擎很啰嗦，stdout 只留给协议
console.error = () => {};
const send = (o) => process.stdout.write(JSON.stringify(o) + '\n');
const sites = new Map();

const J = (x) => { if (typeof x === 'string') { try { return JSON.parse(x); } catch { return {}; } } return x || {}; };
const withTimeout = (p, ms, what) => Promise.race([Promise.resolve(p), new Promise((_, j) => setTimeout(() => j(new Error(what + ' 超时')), ms))]);

let seq = 0;
async function load(site, ext) {
  const cur = sites.get(site);
  if (cur && cur.ext === ext) return cur.eng;
  const mod = await import('./engine.mjs?s=' + (++seq));   // 每个站源一份独立的引擎实例（引擎内部有全局状态）
  const eng = mod.default;
  await withTimeout(eng.init(ext), 30000, 'init');
  sites.set(site, { eng, ext });
  return eng;
}

async function handle(req) {
  const a = req.args || {};
  const eng = await load(req.site, req.ext);
  switch (req.op) {
    case 'home': {
      const h = J(await withTimeout(eng.home(), 30000, 'home'));
      let list = [];
      try { list = J(await withTimeout(eng.homeVod(), 30000, 'homeVod')).list || []; } catch {}
      return { class: h.class || [], filters: h.filters || {}, list };
    }
    case 'category': return J(await withTimeout(eng.category(String(a.tid), Number(a.pg || 1), false, a.extend || {}), 40000, 'category'));
    case 'detail': return J(await withTimeout(eng.detail(String(a.id)), 40000, 'detail'));
    case 'search': return J(await withTimeout(eng.search(a.key, false, Number(a.pg || 1)), 40000, 'search'));
    case 'play': { const r = J(await withTimeout(eng.play(a.flag || '', String(a.id), []), 40000, 'play')); try { r.host = String(eng.host() || ''); } catch {} return r; }
    case 'proxy': {
      const r = await withTimeout(eng.proxy(a.params || {}), 40000, 'proxy');
      if (!r) return { code: 404, mime: 'text/plain', body: '' };
      const body = r[2] == null ? Buffer.alloc(0) : (Buffer.isBuffer(r[2]) ? r[2] : Buffer.from(typeof r[2] === 'string' ? r[2] : JSON.stringify(r[2])));
      return { code: Number(r[0]) || 200, mime: String(r[1] || 'application/octet-stream'), body: body.toString('base64') };
    }
  }
  throw new Error('unknown op ' + req.op);
}

const rl = readline.createInterface({ input: process.stdin });
let chain = Promise.resolve();
rl.on('line', (line) => {
  let req; try { req = JSON.parse(line); } catch { return; }
  // 引擎里是同步阻塞的 curl，同一进程内串行处理；并发靠 Go 端多开几个 worker。
  chain = chain.then(async () => {
    try { send({ id: req.id, ok: true, data: await handle(req) }); }
    catch (e) { send({ id: req.id, ok: false, err: String((e && e.message) || e).slice(0, 300) }); }
  });
});
rl.on('close', () => process.exit(0));
