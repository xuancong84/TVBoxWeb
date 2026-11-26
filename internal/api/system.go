package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/TVBoxWeb/internal/sysconf"
)

// hSystem 系统级参数（超时 / 连接 / 并发 / 缓存 / SSH 跳板 / 解释器）。
//
//	GET    /api/system   参数定义 + 当前值 + 只读启动参数 + 配置文件路径
//	PUT    /api/system   body: {键: 值, ...} 合并保存到 config.json
//	DELETE /api/system   恢复全部默认值
func hSystem(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ok(w, map[string]any{
			"schema":    sysconf.Schema(),
			"values":    sysconf.Values(),
			"info":      sysconf.Info(),
			"listening": sysconf.Listening(),
			"overrides": sysconf.Overrides(),
			"path":      sysconf.Path(),
		})
	case http.MethodPut:
		var m map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&m); err != nil {
			fail(w, 400, "参数格式错误")
			return
		}
		if v, has := m["ssh.jump"]; has {
			s, _ := v.(string)
			if msg := checkJump(s); msg != "" {
				fail(w, 400, msg)
				return
			}
		}
		if err := sysconf.Update(m); err != nil {
			fail(w, 400, err.Error())
			return
		}
		ok(w, map[string]any{"values": sysconf.Values()})
	case http.MethodDelete:
		if err := sysconf.Reset(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		ok(w, map[string]any{"values": sysconf.Values()})
	default:
		fail(w, 405, "method not allowed")
	}
}

// checkJump 跳板命令会被 sh -c 执行：只允许「ssh 开头、不含 shell 元字符」的命令，
// 避免设置页被用来执行任意命令。
func checkJump(s string) string {
	for _, c := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }) {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.HasPrefix(c, "ssh ") {
			return "跳板命令必须以 “ssh ” 开头：" + c
		}
		if strings.ContainsAny(c, "`$&|<>(){}\\\"'*?!#") {
			return "跳板命令含不允许的特殊字符：" + c
		}
	}
	return ""
}

var sshTestMu sync.Mutex

// hSystemSSHTest 逐条测试跳板：在远端执行 echo ok，返回每条的结果与耗时。
// POST /api/system/ssh-test  body: {"jump": "<文本，缺省用当前配置>"}
func hSystemSSHTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	var req struct {
		Jump string `json:"jump"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	jump := strings.TrimSpace(req.Jump)
	if jump == "" {
		jump = sysconf.Str("ssh.jump")
	}
	if msg := checkJump(jump); msg != "" {
		fail(w, 400, msg)
		return
	}
	if !sshTestMu.TryLock() {
		fail(w, 429, "上一次测试还没结束")
		return
	}
	defer sshTestMu.Unlock()
	type row struct {
		Cmd   string `json:"cmd"`
		OK    bool   `json:"ok"`
		Ms    int64  `json:"ms"`
		Error string `json:"error,omitempty"`
	}
	var out []row
	for _, c := range strings.Split(jump, ";") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		run := strings.Replace(c, "ssh ", "ssh -o BatchMode=yes -o ConnectTimeout=8 ", 1)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		t0 := time.Now()
		var buf bytes.Buffer
		cmd := exec.CommandContext(ctx, "sh", "-c", run+" 'echo ok'")
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		cancel()
		rw := row{Cmd: c, Ms: time.Since(t0).Milliseconds()}
		if err == nil && strings.Contains(buf.String(), "ok") {
			rw.OK = true
		} else {
			rw.Error = strings.TrimSpace(buf.String())
			if rw.Error == "" && err != nil {
				rw.Error = err.Error()
			}
			if len(rw.Error) > 300 {
				rw.Error = rw.Error[:300]
			}
		}
		out = append(out, rw)
	}
	ok(w, map[string]any{"results": out})
}

// RestartFn 由 tvbox 注入：用相同命令行参数原地重启进程。
var RestartFn func() error

// hSystemRestart POST /api/system/restart 重启后端（让「需重启」的参数生效）。
func hSystemRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	if RestartFn == nil {
		fail(w, 501, "当前运行方式不支持页面内重启，请手动重启")
		return
	}
	if err := RestartFn(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]any{"restarting": true})
}
