//go:build !windows

package cli

import "syscall"

// nofollow makes OpenFile fail instead of following a symlink in the final
// path component.
const nofollow = syscall.O_NOFOLLOW
