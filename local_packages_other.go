//go:build !windows

package main

func resolveLocalPath(string) (string, bool) {
	return "", false
}
