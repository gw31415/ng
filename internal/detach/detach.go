// Package detach lets ng give the terminal back when started from one,
// as GVim does: "ng file" returns to the shell prompt and the window
// stays. "ng -f" (or --nofork) keeps ng in the foreground, which is what
// a program that waits for its editor (git's core.editor) needs.
//
// Go cannot fork, so ng starts itself again in a new session and exits
// once the window is up. Until then the parent shows what the child
// writes to stderr and, if the child exits early, exits with its status:
// errors such as an unknown option or a missing nvim still reach the
// terminal.
package detach

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// childEnv marks the process started by Background.
const childEnv = "NG_DETACHED"

// Child reports whether this process is the background copy, and prepares
// it: the variable is removed so that Neovim does not inherit it, and
// SIGPIPE is ignored so that writing to stderr after the parent has gone
// fails instead of killing ng. Call it before starting goroutines.
func Child() bool {
	if os.Getenv(childEnv) == "" {
		return false
	}
	os.Unsetenv(childEnv)
	signal.Ignore(syscall.SIGPIPE)
	return true
}

var readyOnce sync.Once

// Ready tells the parent that the window is up: stderr, a pipe to the
// parent, is replaced with /dev/null and the parent exits with status 0.
// Safe to call more than once and in a process that is not a child.
func Ready() {
	readyOnce.Do(func() {
		null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		dupTo(int(null.Fd()), 2)
		null.Close()
	})
}

// FromTerminal reports whether ng was started from a terminal (stdin,
// stdout or stderr is one). From a launcher or a script, there is no
// terminal to give back.
func FromTerminal() bool {
	for fd := 0; fd <= 2; fd++ {
		if isTerminal(fd) {
			return true
		}
	}
	return false
}

func isTerminal(fd int) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// Background starts ng again with the same arguments in a new session and
// returns the exit status for this process: 0 once the child's window is
// up, the child's status if it exits first.
func Background(stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		return -1
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), childEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return run(cmd, stderr)
}

// readyWait is how long the parent waits, after the child closed its
// stderr, to tell a child that exited from one that is ready.
const readyWait = 200 * time.Millisecond

// run starts cmd with its stderr piped to this process and copies it to
// stderr until the child closes it.
func run(cmd *exec.Cmd, stderr io.Writer) int {
	r, w, err := os.Pipe()
	if err != nil {
		return -1
	}
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		w.Close()
		r.Close()
		return -1
	}
	w.Close()
	io.Copy(stderr, r) // until Ready or exit
	r.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		if err != nil {
			return 1
		}
		return 0
	case <-time.After(readyWait):
		// Still running with stderr closed: the window is up. The child
		// is in its own session and outlives this process.
		cmd.Process.Release()
		return 0
	}
}
