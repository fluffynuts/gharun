//go:build !windows

package main

import "errors"

// addToUserPath is only implemented on Windows; elsewhere PATH lives in shell
// startup files, which gharun leaves to the user.
func addToUserPath(dir string) (bool, error) {
	return false, errors.ErrUnsupported
}
