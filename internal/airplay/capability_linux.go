//go:build linux

package airplay

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Keep the container's deliberately granted bind capability through exec.
// A non-root process otherwise loses its effective capabilities when executing
// an ordinary binary. Local users without this capability get normal behavior.
func configureCapabilities(cmd *exec.Cmd) {
	h := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var d [2]unix.CapUserData
	if unix.Capget(&h, &d[0]) == nil && d[0].Permitted&(1<<unix.CAP_NET_BIND_SERVICE) != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: []uintptr{unix.CAP_NET_BIND_SERVICE}}
	}
}
