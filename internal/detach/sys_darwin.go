package detach

import "syscall"

const ioctlGetTermios = syscall.TIOCGETA

func dupTo(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }
