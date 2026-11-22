package crawler

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/xxl6097/TVBoxWeb/internal/httpc"
)

// blobRe 识别「伪装成图片」的地区限制源里嵌入的 base64 配置块：有些源
// （例如饭太硬 /tv 接口）对境外 IP 返回一张干净的占位「图片」，只有对大陆
// IP 才会在同样是「图片」的响应字节里嵌一段 base64 编码的真实 TVBox 配置。
var blobRe = regexp.MustCompile(`[A-Za-z0-9+/=]{200,}`)

// extractUsableConfigText 先直接把拉到的文本当配置解析；解不出站点/直播源
// 就再在原始字节里找一段嵌入的 base64 块，解码后重试一次。
func extractUsableConfigText(text string) (string, bool) {
	if looksUsable(text) {
		return text, true
	}
	m := blobRe.FindString(text)
	if m == "" {
		return "", false
	}
	dec, err := base64.StdEncoding.DecodeString(m)
	if err != nil {
		return "", false
	}
	decoded := string(dec)
	if looksUsable(decoded) {
		return decoded, true
	}
	return "", false
}

func looksUsable(text string) bool {
	cfg, err := parseConfig(text)
	return err == nil && (len(cfg.Sites) > 0 || len(cfg.Lives) > 0)
}

// worthGeoRelay 判断一次直接抓取失败是否值得再花一轮 SSH 跳板去重试。
// 跳板只是换一个出口 IP，对「跟地区无关的失败」毫无帮助，白白多等一轮
// SSH 握手 + 远端超时：
//   - DNS 解析失败（NXDOMAIN/SERVFAIL）：域名在公共 DNS 上就是查不到，
//     换出口 IP 不会改变解析结果；
//   - HTTP 404/401/410/423/429/452/456 等确定性状态码：资源本身不存在、
//     需要鉴权或已被下架限流，不是「仅对某些地区返回不同内容」；
//   - 「重定向目标不安全」：是本机的 SSRF 安全校验拦下的，换跳板会绕过
//     这层校验，不应该跳板重试。
//
// 只有 ferr == nil（抓到内容但解不出配置，典型的「伪装成图片」地区限制）、
// 请求超时（dial/i-o timeout，可能是 GFW 丢包）、或 HTTP 403（最常见的
// 地区拒绝信号）才值得再试一轮跳板。
func worthGeoRelay(ferr error) bool {
	if ferr == nil {
		return true
	}
	msg := ferr.Error()
	if strings.Contains(msg, "不安全") {
		return false
	}
	for _, code := range []string{"HTTP 404", "HTTP 401", "HTTP 410", "HTTP 423", "HTTP 429", "HTTP 452", "HTTP 456"} {
		if strings.Contains(msg, code) {
			return false
		}
	}
	if strings.Contains(msg, "lookup") && (strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving")) {
		return false
	}
	if strings.Contains(msg, "connection refused") {
		return false
	}
	return true
}

// fetchViaGeoBlockRelays 依次尝试环境变量 SSH_CMDS_GEO_BLOCK 里配置的每条
// SSH 命令（用 ";" 分隔，比如家里另一台在目标地区的机器），通过它在远端
// 执行 curl 抓取 url，直到某一条跳板返回的内容能解析出站点/直播源（或从中
// 解出嵌入的 base64 配置块）为止，用来绕开「只对特定地区 IP 返回正常内容」
// 的订阅源。没配这个环境变量时直接报错，调用方会把原始错误原样抛给用户。
func fetchViaGeoBlockRelays(ctx context.Context, url string) (string, error) {
	raw := strings.TrimSpace(os.Getenv("SSH_CMDS_GEO_BLOCK"))
	if raw == "" {
		return "", errors.New("未配置 SSH_CMDS_GEO_BLOCK，无法尝试跳板中转")
	}
	remoteCurl := "curl -sL -A " + shQuote(httpc.UA) + " --max-time 20 " + shQuote(url)
	var lastErr = errors.New("所有跳板都没能取到可用内容")
	for _, sshCmd := range strings.Split(raw, ";") {
		sshCmd = strings.TrimSpace(sshCmd)
		if sshCmd == "" {
			continue
		}
		// 同一轮展开里同一条跳板会被反复调用（每个失败的地址都要跳板重试
		// 一次），每次都新开一条 SSH 连接光握手就要 ~2s。给它接上
		// ControlMaster，第一次之后的调用复用同一条连接，握手开销趋近于 0。
		sshCmd = withControlMaster(sshCmd)
		full := sshCmd + " " + shQuote(remoteCurl)
		cmd := exec.CommandContext(ctx, "sh", "-c", full)
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			lastErr = errors.New(sshCmd + "：" + err.Error())
			continue
		}
		if usable, ok := extractUsableConfigText(out.String()); ok {
			return usable, nil
		}
		lastErr = errors.New(sshCmd + "：跳板取到的内容不是合法配置")
	}
	return "", lastErr
}

// shQuote 把字符串包成安全的单引号 shell 字面量，防止里面的空格/引号
// 破坏外层 `sh -c "..."` 拼出来的命令行。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// withControlMaster 只给形如 "ssh ..." 的跳板命令插入连接复用选项（必须插在
// "ssh" 紧后面——这些选项要出现在目的地参数之前，ssh 才认）。不是 ssh 的
// 跳板命令（比如本地脚本）原样返回。ControlPath 按跳板命令本身的哈希区分，
// 避免多条不同跳板互相抢同一个 socket 文件。
func withControlMaster(sshCmd string) string {
	if !strings.HasPrefix(sshCmd, "ssh ") {
		return sshCmd
	}
	h := sha1.Sum([]byte(sshCmd))
	sock := "/tmp/tvboxweb-sshcm-" + hex.EncodeToString(h[:])[:12]
	opts := "-o ControlMaster=auto -o ControlPersist=60s -o ControlPath=" + sock
	return "ssh " + opts + strings.TrimPrefix(sshCmd, "ssh")
}

// WorthGeoRelay 是 worthGeoRelay 的导出版本，给 live 等包判断直连失败后是否值得走跳板。
func WorthGeoRelay(ferr error) bool { return worthGeoRelay(ferr) }

// FetchSmallViaGeoRelay 通过 SSH_CMDS_GEO_BLOCK 跳板抓取一个「小文件」（m3u8 播放列表、
// AES 密钥）。只给 /api/proxy 在直连被地区封锁时兜底用：返回内容超过 maxBytes 会被
// 丢弃并报错，所以即使调用方误传了视频分片地址，也不会通过 SSH 搬运视频主体数据。
func FetchSmallViaGeoRelay(ctx context.Context, url string, maxBytes int) ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("SSH_CMDS_GEO_BLOCK"))
	if raw == "" {
		return nil, errors.New("未配置 SSH_CMDS_GEO_BLOCK")
	}
	remoteCurl := "curl -sfL -A " + shQuote(httpc.UA) + " --max-time 20 --max-filesize " +
		strconv.Itoa(maxBytes) + " " + shQuote(url)
	lastErr := errors.New("所有跳板都没能取到内容")
	for _, sshCmd := range strings.Split(raw, ";") {
		sshCmd = strings.TrimSpace(sshCmd)
		if sshCmd == "" {
			continue
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", withControlMaster(sshCmd)+" "+shQuote(remoteCurl))
		var out bytes.Buffer
		cmd.Stdout = &limitWriter{w: &out, n: maxBytes + 1}
		if err := cmd.Run(); err != nil {
			lastErr = errors.New("跳板抓取失败：" + err.Error())
			continue
		}
		if out.Len() == 0 || out.Len() > maxBytes {
			lastErr = errors.New("跳板返回内容为空或超出大小限制")
			continue
		}
		return out.Bytes(), nil
	}
	return nil, lastErr
}

// limitWriter 超过 n 字节后报错，让 cmd.Run 提前结束（同时杀掉远端 curl）。
type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.w.Len()+len(p) > l.n {
		return 0, errors.New("超出大小限制")
	}
	return l.w.Write(p)
}
