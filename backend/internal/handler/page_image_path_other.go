//go:build !windows

package handler

import "path/filepath"

func resolvePageImagePhysicalPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
