# Python 站源 worker：由 Go 端常驻启动，通过 stdin/stdout 按「一行一个 JSON」通信。
#   请求  {"id":1,"op":"home|category|detail|search|play|proxy","site":"key","file":"/path/x.py","ext":"...","args":{...}}
#   响应  {"id":1,"ok":true,"data":...} 或 {"id":1,"ok":false,"err":"..."}
import base64, concurrent.futures as cf, importlib.util, json, os, sys, threading, traceback

ROOT = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, ROOT)

proto = os.fdopen(os.dup(1), 'w', buffering=1, encoding='utf-8')
sys.stdout = open(os.devnull, 'w')   # 站源里的 print 不能污染协议通道
sys.stderr = open(os.devnull, 'w')

wlock = threading.Lock()
spiders = {}          # site key -> {"sp": instance, "lock": Lock, "file": path, "ext": ext}
spiders_lock = threading.Lock()


def reply(obj):
    with wlock:
        proto.write(json.dumps(obj, ensure_ascii=False, default=str) + '\n')
        proto.flush()


def load(site, path, ext):
    with spiders_lock:
        cur = spiders.get(site)
        if cur and cur['file'] == path and cur['ext'] == ext:
            return cur
        ent = {'file': path, 'ext': ext, 'lock': threading.Lock(), 'sp': None}
        spiders[site] = ent
    with ent['lock']:
        d = os.path.dirname(path)
        if d not in sys.path:
            sys.path.append(d)
        spec = importlib.util.spec_from_file_location('spider_' + str(abs(hash(path))), path)
        m = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(m)
        sp = m.Spider()
        sp.init(ext)
        ent['sp'] = sp
    return ent


def to_bytes(x):
    if x is None:
        return b''
    if isinstance(x, bytes):
        return x
    if isinstance(x, str):
        return x.encode('utf-8')
    if hasattr(x, 'read'):
        return x.read(64 << 20)
    return b''.join(to_bytes(i) for i in x)


def handle(req):
    op, site, a = req['op'], req['site'], req.get('args') or {}
    ent = load(site, req['file'], req.get('ext', ''))
    with ent['lock']:
        sp = ent['sp']
        if op == 'home':
            h = sp.homeContent(True) or {}
            try:
                hv = sp.homeVideoContent() or {}
            except Exception:
                hv = {}
            return {'class': h.get('class') or [], 'filters': h.get('filters') or {}, 'list': h.get('list') or hv.get('list') or []}
        if op == 'category':
            return sp.categoryContent(str(a['tid']), str(a.get('pg', 1)), False, a.get('extend') or {}) or {}
        if op == 'detail':
            return sp.detailContent([str(a['id'])]) or {}
        if op == 'search':
            try:
                return sp.searchContent(a['key'], False, str(a.get('pg', 1))) or {}
            except TypeError:
                return sp.searchContent(a['key'], False) or {}
        if op == 'play':
            return sp.playerContent(a.get('flag', ''), str(a['id']), []) or {}
        if op == 'proxy':
            r = sp.localProxy(a.get('params') or {})
            if not r:
                return {'code': 404, 'mime': 'text/plain', 'body': ''}
            code = int(r[0]) if str(r[0]).isdigit() else 200
            mime = str(r[1]) if len(r) > 1 and r[1] else 'application/octet-stream'
            body = to_bytes(r[2]) if len(r) > 2 else b''
            return {'code': code, 'mime': mime, 'body': base64.b64encode(body).decode()}
    raise ValueError('unknown op ' + op)


def run(req):
    try:
        reply({'id': req['id'], 'ok': True, 'data': handle(req)})
    except BaseException as e:
        reply({'id': req['id'], 'ok': False, 'err': '%s: %s' % (type(e).__name__, str(e)[:300])})


def main():
    pool = cf.ThreadPoolExecutor(int(os.environ.get('PYSPIDER_THREADS', '24')))
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception:
            continue
        pool.submit(run, req)


if __name__ == '__main__':
    main()
