//go:build windows

package tvbox

import "errors"

func reexec(exe string, args []string) error {
	return errors.New("此平台不支持页面内重启，请手动重启")
}
