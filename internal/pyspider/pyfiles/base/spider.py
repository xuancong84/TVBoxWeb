# 最小实现的 TVBox「base.spider」基类：Python 站源（继承 Spider）运行所需的公共能力。
# 由 Go 端（internal/pyspider）在启动时释放到数据目录，worker.py 通过 sys.path 导入。
import base64, ipaddress, json, os, re, socket, time, urllib.parse, warnings
import requests
from lxml import etree

warnings.filterwarnings('ignore')
try:
    requests.packages.urllib3.disable_warnings()
except Exception:
    pass

# 这些「本地代理」主机名是 TVBox 客户端的约定（站源用 getProxyUrl() 拼出自己的代理地址），
# 在这里既不是真的联网目标，也不该被当成内网访问拦掉（它们会被 Go 端截获处理）。
_LOCAL_PROXY_PORTS = {9978, 9877, 10010}


def _check_url(url):
    """尽力而为的 SSRF 防护：站源代码是第三方的，禁止它们访问内网/本机服务。"""
    try:
        p = urllib.parse.urlparse(url)
        host = p.hostname
        if not host or p.scheme not in ('http', 'https'):
            return
        ips = [a[4][0] for a in socket.getaddrinfo(host, None)]
    except Exception:
        return
    for ip in ips:
        try:
            a = ipaddress.ip_address(ip)
        except ValueError:
            continue
        if a.is_private or a.is_loopback or a.is_link_local or a.is_reserved or a.is_multicast:
            raise PermissionError('blocked private address: ' + host)


_orig_request = requests.Session.request

# 地区封锁兜底：直连失败（超时/被重置/403/451）时，让 Go 端通过 SSH_CMDS_GEO_BLOCK 跳板重试一次。
# 环境变量 SPIDER_RELAY 是 Go 在本机开的中转接口；SSH 命令和密钥不在这个进程里。
_RELAY = os.environ.get('SPIDER_RELAY', '')
_NO_RELAY_HINTS = ('Name or service not known', 'NameResolutionError', 'nodename nor servname', 'No address associated',
                   'Connection refused', 'blocked private address')


def _relay(session, method, url, kw):
    try:
        req = requests.Request(method, url, params=kw.get('params'), data=kw.get('data'), json=kw.get('json'),
                               headers=kw.get('headers'), cookies=kw.get('cookies'))
        p = session.prepare_request(req)
        body = p.body
        if isinstance(body, str):
            body = body.encode('utf-8')
        payload = {'url': p.url, 'method': p.method, 'headers': {k: str(v) for k, v in p.headers.items()},
                   'body': base64.b64encode(body).decode() if body else ''}
        rr = _orig_request(requests.Session(), 'POST', _RELAY, json=payload, timeout=45)
        d = rr.json()
        if d.get('err') or not d.get('status'):
            return None
        r = requests.Response()
        r.status_code = int(d['status'])
        r._content = base64.b64decode(d.get('body') or '')
        r.url = p.url
        r.encoding = 'utf-8'
        for k, v in (d.get('headers') or {}).items():
            r.headers[k] = v
        return r
    except Exception:
        return None


_relay_hosts = {}   # 直连失败过的主机 -> 「改走跳板」的截止时间：后续请求不再先白等一轮直连超时
_RELAY_MEMORY = 600


def _guarded_request(self, method, url, *a, **kw):
    _check_url(url)
    kw.setdefault('verify', False)
    if kw.get('timeout') is None:
        kw['timeout'] = 15
    host = urllib.parse.urlparse(url).netloc
    if _RELAY and _relay_hosts.get(host, 0) > time.time():
        rr = _relay(self, method, url, kw)
        if rr is not None:
            return rr
    try:
        r = _orig_request(self, method, url, *a, **kw)
    except (requests.exceptions.ConnectionError, requests.exceptions.Timeout, requests.exceptions.SSLError) as e:
        if _RELAY and not any(h in str(e) for h in _NO_RELAY_HINTS):
            rr = _relay(self, method, url, kw)
            if rr is not None:
                _relay_hosts[host] = time.time() + _RELAY_MEMORY
                return rr
        raise
    if _RELAY and r.status_code in (403, 451):
        rr = _relay(self, method, url, kw)
        if rr is not None and rr.status_code < 400:
            return rr
    return r


requests.Session.request = _guarded_request


class Spider(object):
    def __init__(self):
        self.extend = ''

    def init(self, extend=''):
        pass

    def getName(self):
        return ''

    def homeContent(self, filter):
        return {}

    def homeVideoContent(self):
        return {}

    def categoryContent(self, tid, pg, filter, extend):
        return {}

    def detailContent(self, ids):
        return {}

    def searchContent(self, key, quick, pg='1'):
        return {}

    def playerContent(self, flag, id, vipFlags):
        return {}

    def localProxy(self, param):
        return None

    def isVideoFormat(self, url):
        return False

    def manualVideoCheck(self):
        return False

    def destroy(self):
        pass

    def action(self, action):
        return None

    def liveContent(self, url):
        return ''

    def getProxyUrl(self, local=True):
        return 'http://127.0.0.1:9978/proxy?do=py'

    def getDependence(self):
        return []

    def log(self, msg):
        pass

    def _sess(self):
        if not hasattr(self, '_s'):
            self._s = requests.Session()
        return self._s

    def fetch(self, url, params=None, cookies=None, headers=None, timeout=5, verify=True, stream=False, allow_redirects=True):
        r = self._sess().get(url, params=params, cookies=cookies, headers=headers, timeout=max(timeout, 10),
                             verify=False, stream=stream, allow_redirects=allow_redirects)
        r.encoding = 'utf-8'
        return r

    def post(self, url, params=None, data=None, json=None, cookies=None, headers=None, timeout=5, verify=True, stream=False, allow_redirects=True):
        r = self._sess().post(url, params=params, data=data, json=json, cookies=cookies, headers=headers,
                              timeout=max(timeout, 10), verify=False, stream=stream, allow_redirects=allow_redirects)
        r.encoding = 'utf-8'
        return r

    def html(self, content):
        return etree.HTML(content)

    def str2json(self, s):
        return json.loads(s)

    def json2str(self, j):
        return json.dumps(j, ensure_ascii=False)

    def cleanText(self, src):
        return re.sub(r'[\U0001F600-\U0001F64F\U0001F300-\U0001F5FF\U0001F680-\U0001F6FF\U0001F1E0-\U0001F1FF]', '', src)

    def removeHtmlTags(self, src):
        return re.sub(re.compile('<.*?>'), '', src)

    def regStr(self, src, reg, group=1):
        m = re.search(reg, src)
        return m.group(group) if m else ''

    _cache = {}

    def setCache(self, key, value):
        self._cache[key] = value

    def getCache(self, key):
        return self._cache.get(key)

    def delCache(self, key):
        self._cache.pop(key, None)

    def getFlags(self):
        return []
