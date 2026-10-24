package shell

import "syscall"

// alive reports whether a process with this id still exists (and is not a zombie
// we could not have reaped, which signal 0 still counts: the test's sleep is
// adopted by init, so a killed one disappears).
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
