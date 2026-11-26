package pyspider

import "testing"

func TestSpec(t *testing.T) {
	cases := []struct {
		api, ext, base string
		url, init      string
		ok             bool
	}{
		{"./py/a.py", "", "https://x.com/cfg/s.json", "https://x.com/cfg/py/a.py", "", true},
		{"./py/a.py", `{"k":1}`, "https://x.com/s.json", "https://x.com/py/a.py", `{"k":1}`, true},
		{"https://h/b.py?x=1", "e", "", "https://h/b.py", "e", true},
		{"py_cctv_full", "http://h/c.py", "", "http://h/c.py", "", true},
		{"csp_XBPQ", "./a.json", "", "", "", false},
		{"./lib/drpy2.min.js", "./js/a.js", "", "", "", false},
	}
	for _, c := range cases {
		u, e, ok := Spec(c.api, c.ext, c.base)
		if ok != c.ok || (ok && (u != c.url || e != c.init)) {
			t.Errorf("Spec(%q,%q,%q) = %q,%q,%v; want %q,%q,%v", c.api, c.ext, c.base, u, e, ok, c.url, c.init, c.ok)
		}
	}
	if IsPySite(1, "./a.py", "") {
		t.Error("type 1 must not be a py site")
	}
	if !IsPySite(3, "./a.py", "") || IsPySite(3, "csp_X", "") {
		t.Error("IsPySite wrong")
	}
}

func TestTokens(t *testing.T) {
	tok := PlayToken("站 key/1", "线路:A", "http://a/b?c=d$e")
	for _, bad := range []string{"://", "."} {
		if contains(tok, bad) {
			t.Fatalf("token %q contains %q", tok, bad)
		}
	}
	k, f, id, ok := ParsePlayToken(tok)
	if !ok || k != "站 key/1" || f != "线路:A" || id != "http://a/b?c=d$e" {
		t.Fatalf("roundtrip failed: %q %q %q %v", k, f, id, ok)
	}
	if _, _, _, ok := ParsePlayToken("pyplay:x:y"); ok {
		t.Error("malformed token accepted")
	}
	pt := ProxyToken("k", "do=py&type=m3u8&url=http%3A%2F%2Fa")
	k, q, ok := ParseProxyToken(pt)
	if !ok || k != "k" || q != "do=py&type=m3u8&url=http%3A%2F%2Fa" {
		t.Fatalf("proxy token roundtrip failed: %q %q", k, q)
	}
}

func TestLocalProxyQuery(t *testing.T) {
	if q, ok := LocalProxyQuery("http://127.0.0.1:9978/proxy?do=py&type=m3u8"); !ok || q != "do=py&type=m3u8" {
		t.Errorf("got %q %v", q, ok)
	}
	for _, u := range []string{"http://example.com/proxy?a=1", "http://127.0.0.1:9978/other", "http://10.0.0.1/proxy?x"} {
		if _, ok := LocalProxyQuery(u); ok {
			t.Errorf("%s should not be a local proxy url", u)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
