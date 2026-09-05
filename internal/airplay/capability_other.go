//go:build !linux

package airplay

import "os/exec"

func configureCapabilities(cmd *exec.Cmd) {}
