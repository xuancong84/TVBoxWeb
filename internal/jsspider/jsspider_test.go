package jsspider

import "testing"

func TestSpec(t *testing.T) {
	cases := []struct {
		api, ext, base, url, init string
		ok                        bool
	}{
		{"./lib/drpy2.min.js", "./js/a.js", "https://x.com/cfg/s.json", "https://x.com/cfg/js/a.js", "", true},
		{"https://h/libs/drpy2.min.js", "https://r/x.js", "", "https://r/x.js", "", true},
		{"./js/drpy2.min.js", "./js/采集.js?type=url&params=http://j", "https://x.com/s.json", "https://x.com/js/%E9%87%87%E9%9B%86.js", "https://x.com/js/%E9%87%87%E9%9B%86.js?type=url&params=http://j", true},
		{"./lib/drpy.min.js", "./a.js", "https://x.com/s.json", "https://x.com/a.js", "", true},
		{"csp_XBPQ", "./a.js", "", "", "", false},
		{"./js/alist.min.js", "./a.json", "", "", "", false},
		{"./lib/drpy2.min.js", "./a.json", "", "", "", false},
	}
	for _, c := range cases {
		u, e, ok := Spec(c.api, c.ext, c.base)
		if ok != c.ok || (ok && (u != c.url || e != c.init)) {
			t.Errorf("Spec(%q,%q,%q) = %q,%q,%v; want %q,%q,%v", c.api, c.ext, c.base, u, e, ok, c.url, c.init, c.ok)
		}
	}
}
