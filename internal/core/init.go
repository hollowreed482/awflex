package core

import (
	"os"
	"os/signal"
	"syscall"
)

// RunAsInit is used when awflex is process 1 of a container. Process 1 must
// collect every orphaned process, and ansible leaves ssh processes behind, so
// without this they pile up as zombies. It starts the real server as a child,
// passes signals on, reaps everything, and exits with the server's status.
// This keeps awflex independent of an init binary in the base image.
func RunAsInit() int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	child, err := os.StartProcess(self, os.Args, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return 1
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for s := range sigs {
			child.Signal(s)
		}
	}()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1
		}
		if pid == child.Pid {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
	}
}
