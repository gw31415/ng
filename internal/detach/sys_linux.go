package detach

import "syscall"

const ioctlGetTermios = syscall.TCGETS

func dupTo(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }
