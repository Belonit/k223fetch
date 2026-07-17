//go:build windows

package main

import (
	"os"
	"strings"
)

func resolveLocalPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	programFilesX86 := os.Getenv("ProgramFiles(x86)")
	if programFilesX86 == "" {
		programFilesX86 = `C:\Program Files (x86)`
	}
	return strings.ReplaceAll(path, `%ProgramFiles(x86)%`, programFilesX86), true
}
