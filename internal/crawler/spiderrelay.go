package crawler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/httpc"
	"github.com/xxl6097/TVBoxWeb/internal/netguard"
)

// StartSpiderRelay 在本机回环地址上起一个「跳板中转」小服务，供 Python/JS 站源 worker 使用：
// 站源脚本直连被地区封锁时，让 Go 通过 SSH_CMDS_GEO_BLOCK 跳板代为请求。
// 这样 SSH 命令和密钥始终留在 Go 进程里，第三方脚本碰不到；中转只搬 ≤2MB 的接口/网页数据，
// 并做 SSRF 校验——视频主体数据绝不走 SSH。
// 返回给 worker 用的地址（带随机密钥路径）；没配 SSH_CMDS_GEO_BLOCK 返回空串。
func StartSpiderRelay() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ""
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	secret := hex.EncodeToString(b)
	mux := http.NewServeMux()
	mux.HandleFunc("/"+secret, handleSpiderRelay)
	go func() { _ = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln) }()
	return "http://" + ln.Addr().String() + "/" + secret
}

const spiderRelayMax = 2 << 20

type relayReq struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64
}

type relayResp struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64
	Err     string            `json:"err,omitempty"`
}

func handleSpiderRelay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req relayReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		_ = json.NewEncoder(w).Encode(relayResp{Err: "bad request"})
		return
	}
	resp, err := relayViaGeoBlock(r.Context(), req)
	if err != nil {
		_ = json.NewEncoder(w).Encode(relayResp{Err: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func relayViaGeoBlock(ctx context.Context, req relayReq) (relayResp, error) {
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		return relayResp{}, errors.New("only http(s)")
	}
	if err := netguard.Check(req.URL); err != nil {
		return relayResp{}, errors.New("blocked: " + err.Error())
	}
	raw := strings.TrimSpace(sysconf.Str("ssh.jump"))
	if raw == "" {
		return relayResp{}, errors.New("no relay configured")
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "POST" && method != "HEAD" {
		return relayResp{}, errors.New("method not allowed")
	}
	args := []string{"curl", "-s", "-i", "-L", "--max-redirs", "6", "--compressed", "--max-time", "20",
		"--max-filesize", strconv.Itoa(spiderRelayMax), "--proto", "=http,https", "-X", method}
	hasUA := false
	for k, v := range req.Headers {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-length" || lk == "accept-encoding" || lk == "connection" {
			continue
		}
		if lk == "user-agent" {
			hasUA = true
		}
		args = append(args, "-H", k+": "+v)
	}
	if !hasUA {
		args = append(args, "-A", httpc.WebUA)
	}
	if req.Body != "" {
		body, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil || len(body) > 1<<20 {
			return relayResp{}, errors.New("bad body")
		}
		args = append(args, "--data-binary", string(body))
	}
	args = append(args, req.URL)
	var quoted []string
	for _, a := range args {
		quoted = append(quoted, shQuote(a))
	}
	remote := strings.Join(quoted, " ")
	lastErr := errors.New("所有跳板都没能取到内容")
	for _, sshCmd := range strings.Split(raw, ";") {
		sshCmd = strings.TrimSpace(sshCmd)
		if sshCmd == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, sysconf.Dur("to.geo_file")+5*time.Second)
		cmd := exec.CommandContext(cctx, "sh", "-c", withControlMaster(sshCmd)+" "+shQuote(remote))
		var out bytes.Buffer
		cmd.Stdout = &limitWriter{w: &out, n: spiderRelayMax + 64<<10}
		err := cmd.Run()
		cancel()
		if err != nil && out.Len() == 0 {
			lastErr = errors.New("跳板抓取失败：" + err.Error())
			continue
		}
		if resp, ok := parseCurlI(out.Bytes()); ok {
			return resp, nil
		}
		lastErr = errors.New("跳板返回内容无法解析")
	}
	return relayResp{}, lastErr
}

// parseCurlI 解析 `curl -i -L` 的输出：取最后一组响应头 + 正文。
func parseCurlI(b []byte) (relayResp, bool) {
	rest := b
	var head []byte
	for bytes.HasPrefix(rest, []byte("HTTP/")) {
		i := bytes.Index(rest, []byte("\r\n\r\n"))
		if i < 0 {
			break
		}
		head, rest = rest[:i], rest[i+4:]
		if !bytes.HasPrefix(rest, []byte("HTTP/")) {
			break
		}
	}
	if head == nil {
		return relayResp{}, false
	}
	lines := strings.Split(string(head), "\r\n")
	f := strings.Fields(lines[0])
	if len(f) < 2 {
		return relayResp{}, false
	}
	code, _ := strconv.Atoi(f[1])
	hs := map[string]string{}
	for _, l := range lines[1:] {
		if k, v, ok := strings.Cut(l, ":"); ok {
			lk := strings.ToLower(strings.TrimSpace(k))
			if lk == "content-encoding" || lk == "transfer-encoding" || lk == "content-length" {
				continue
			}
			hs[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return relayResp{Status: code, Headers: hs, Body: base64.StdEncoding.EncodeToString(rest)}, true
}
