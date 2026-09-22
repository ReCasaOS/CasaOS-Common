//go:build !linux

package command

import "os/exec"

// killGroupOnCancel keeps the default of os/exec: a timeout kills the script's
// own process only, and WaitDelay stops waiting for the output its children
// may keep open. Process groups are a Linux affair here; CasaOS runs there.
func killGroupOnCancel(*exec.Cmd) {}
