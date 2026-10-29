//go:build windows

package handler

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	pageImageInitialPathBuffer = 256
	pageImageMaxPathBuffer     = 32768
	pageImageExtendedPrefix    = `\\?\`
	pageImageExtendedUNCPrefix = `\\?\UNC\`
)

func resolvePageImagePhysicalPath(path string) (string, error) {
	extendedPath, err := normalizeWindowsPageImagePath(path)
	if err != nil {
		return "", err
	}
	pathPointer, err := windows.UTF16PtrFromString(extendedPath)
	if err != nil {
		return "", err
	}

	handle, err := windows.CreateFile(
		pathPointer,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("open page image path metadata handle: %w", err)
	}

	finalPath, pathErr := pageImageFinalPathFromHandle(handle)
	closeErr := windows.CloseHandle(handle)
	if pathErr != nil {
		return "", pathErr
	}
	if closeErr != nil {
		return "", fmt.Errorf("close page image path metadata handle: %w", closeErr)
	}
	return finalPath, nil
}

func normalizeWindowsPageImagePath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", errors.New("page image path is empty or contains NUL")
	}
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\??\`) {
		return "", errors.New("page image device and NT paths are unsupported")
	}

	var absolute string
	if isExtendedWindowsDOSPath(path) || isExtendedWindowsUNCPath(path) {
		absolute = filepath.Clean(path)
	} else {
		var err error
		absolute, err = filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("make page image path absolute: %w", err)
		}
		absolute = filepath.Clean(absolute)
	}

	switch {
	case isExtendedWindowsDOSPath(absolute):
		return absolute, nil
	case isExtendedWindowsUNCPath(absolute):
		uncPath := pageImageExtendedUNCPrefix + absolute[len(pageImageExtendedUNCPrefix):]
		if !hasWindowsUNCShare(uncPath[len(pageImageExtendedUNCPrefix):]) {
			return "", errors.New("page image UNC path must include a server and share")
		}
		return uncPath, nil
	case strings.HasPrefix(absolute, `\\`):
		uncPath := strings.TrimPrefix(absolute, `\\`)
		if !hasWindowsUNCShare(uncPath) {
			return "", errors.New("page image UNC path must include a server and share")
		}
		return pageImageExtendedUNCPrefix + uncPath, nil
	case isWindowsDriveAbsolutePath(absolute):
		return pageImageExtendedPrefix + absolute, nil
	default:
		return "", errors.New("page image path is not an absolute DOS or UNC path")
	}
}

func pageImageFinalPathFromHandle(handle windows.Handle) (string, error) {
	bufferSize := pageImageInitialPathBuffer
	for {
		buffer := make([]uint16, bufferSize)
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if length == 0 {
			if err != nil {
				return "", fmt.Errorf("get final page image path: %w", err)
			}
			return "", errors.New("get final page image path returned an empty result")
		}
		if length >= uint32(len(buffer)) {
			if bufferSize >= pageImageMaxPathBuffer || length >= pageImageMaxPathBuffer {
				return "", fmt.Errorf("final page image path exceeds %d UTF-16 units", pageImageMaxPathBuffer)
			}
			nextSize := int(length) + 1
			if nextSize <= bufferSize {
				nextSize = bufferSize * 2
			}
			if nextSize > pageImageMaxPathBuffer {
				nextSize = pageImageMaxPathBuffer
			}
			bufferSize = nextSize
			continue
		}

		finalPath := windows.UTF16ToString(buffer[:length])
		if !isExtendedWindowsDOSPath(finalPath) && !isExtendedWindowsUNCPath(finalPath) {
			return "", fmt.Errorf("final page image path has an unsupported representation: %q", finalPath)
		}
		return finalPath, nil
	}
}

func isExtendedWindowsDOSPath(path string) bool {
	if len(path) < len(pageImageExtendedPrefix)+3 || !strings.HasPrefix(strings.ToUpper(path), strings.ToUpper(pageImageExtendedPrefix)) {
		return false
	}
	drive := path[len(pageImageExtendedPrefix)]
	return isWindowsDriveLetter(drive) && path[len(pageImageExtendedPrefix)+1] == ':' && path[len(pageImageExtendedPrefix)+2] == '\\'
}

func isExtendedWindowsUNCPath(path string) bool {
	return len(path) > len(pageImageExtendedUNCPrefix) && strings.HasPrefix(strings.ToUpper(path), strings.ToUpper(pageImageExtendedUNCPrefix))
}

func isWindowsDriveAbsolutePath(path string) bool {
	return len(path) >= 3 && isWindowsDriveLetter(path[0]) && path[1] == ':' && path[2] == '\\'
}

func isWindowsDriveLetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func hasWindowsUNCShare(path string) bool {
	parts := strings.Split(strings.Trim(path, `\`), `\`)
	return len(parts) >= 2 && parts[0] != "" && parts[1] != ""
}
