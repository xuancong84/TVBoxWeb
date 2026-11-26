package dnscache

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestParseIPs(t *testing.T) {
	out := "Server:  1.1.1.1\nAddress: 1.1.1.1#53\n\nName: a.com\nAddress: 93.184.216.34\nAddress: 2606:2800::1\n"
	got := parseIPs(out)
	if len(got) != 2 || got[0] != "93.184.216.34" {
		t.Fatalf("got %v", got)
	}
	got = parseIPs("93.184.216.34 STREAM a.com\n93.184.216.34 DGRAM\n")
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

// 缓存命中：域名不可解析也能连上缓存的 IP。
func TestDialFromCache(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	put("no-such-host.invalid", []string{"127.0.0.1"}, "dns")
	dial := NewDialContext(&net.Dialer{Timeout: 2 * time.Second})
	c, err := dial(context.Background(), "tcp", "no-such-host.invalid:"+port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
