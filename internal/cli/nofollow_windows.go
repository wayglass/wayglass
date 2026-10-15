//go:build windows

package cli

// Windows has no O_NOFOLLOW; writeFileNoFollow's Lstat check stands alone.
const nofollow = 0
