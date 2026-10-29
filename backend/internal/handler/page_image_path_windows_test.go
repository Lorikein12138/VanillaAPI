//go:build windows

package handler

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestResolvePageImagePathWindowsJunction(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, root string)
	}{
		{
			name: "nested-outside",
			run: func(t *testing.T, root string) {
				pagesDir := filepath.Join(root, "pages")
				base := filepath.Join(pagesDir, "guide")
				outside := filepath.Join(root, "outside")
				if err := os.MkdirAll(base, 0755); err != nil {
					t.Fatalf("create page directory: %v", err)
				}
				writePageImageFixture(t, filepath.Join(outside, "secret.png"), "nested-outside-secret")
				createPageImageWindowsJunction(t, root, filepath.Join(base, "images"), outside)
				assertPageImagePathRejected(t, pagesDir, base, "images/secret.png")
			},
		},
		{
			name: "page-root-outside",
			run: func(t *testing.T, root string) {
				pagesDir := filepath.Join(root, "pages")
				base := filepath.Join(pagesDir, "guide")
				outside := filepath.Join(root, "outside")
				if err := os.MkdirAll(pagesDir, 0755); err != nil {
					t.Fatalf("create pages directory: %v", err)
				}
				writePageImageFixture(t, filepath.Join(outside, "images", "secret.png"), "page-root-outside-secret")
				createPageImageWindowsJunction(t, root, base, outside)
				assertPageImagePathRejected(t, pagesDir, base, "images/secret.png")
			},
		},
		{
			name: "chain-outside",
			run: func(t *testing.T, root string) {
				pagesDir := filepath.Join(root, "pages")
				base := filepath.Join(pagesDir, "guide")
				imagesTarget := filepath.Join(base, "hop")
				outside := filepath.Join(root, "outside")
				if err := os.MkdirAll(imagesTarget, 0755); err != nil {
					t.Fatalf("create first junction target: %v", err)
				}
				writePageImageFixture(t, filepath.Join(outside, "secret.png"), "chain-outside-secret")
				createPageImageWindowsJunction(t, root, filepath.Join(base, "images"), imagesTarget)
				createPageImageWindowsJunction(t, root, filepath.Join(imagesTarget, "external"), outside)
				assertPageImagePathRejected(t, pagesDir, base, "images/external/secret.png")
			},
		},
		{
			name: "within-page",
			run: func(t *testing.T, root string) {
				pagesDir := filepath.Join(root, "pages")
				base := filepath.Join(pagesDir, "guide")
				imagesDir := filepath.Join(base, "images")
				realImages := filepath.Join(imagesDir, "real")
				if err := os.MkdirAll(realImages, 0755); err != nil {
					t.Fatalf("create internal image directory: %v", err)
				}
				writePageImageFixture(t, filepath.Join(realImages, "inside.png"), "within-page-image")
				createPageImageWindowsJunction(t, root, filepath.Join(imagesDir, "linked"), realImages)
				resolved, ok := resolvePageImagePath(pagesDir, imagesDir, "linked/inside.png")
				if !ok {
					t.Fatal("expected an in-page junction target to be accepted")
				}
				assertResolvedPageImageMatchesFile(t, resolved, filepath.Join(realImages, "inside.png"))
			},
		},
		{
			name: "configured-pages-root",
			run: func(t *testing.T, root string) {
				physicalPages := filepath.Join(root, "physical-pages")
				pagesAlias := filepath.Join(root, "pages-alias")
				base := filepath.Join(pagesAlias, "guide")
				image := filepath.Join(base, "images", "inside.png")
				writePageImageFixture(t, filepath.Join(physicalPages, "guide", "images", "inside.png"), "configured-root-image")
				createPageImageWindowsJunction(t, root, pagesAlias, physicalPages)
				resolved, ok := resolvePageImagePath(pagesAlias, base, "images/inside.png")
				if !ok {
					t.Fatal("expected the configured pages-root junction to establish the physical trust root")
				}
				assertResolvedPageImageMatchesFile(t, resolved, image)
			},
		},
		{
			name: "dangling",
			run: func(t *testing.T, root string) {
				pagesDir := filepath.Join(root, "pages")
				base := filepath.Join(pagesDir, "guide")
				target := filepath.Join(root, "dangling-target")
				if err := os.MkdirAll(base, 0755); err != nil {
					t.Fatalf("create page directory: %v", err)
				}
				proofName := createPageImageWindowsJunction(t, root, filepath.Join(base, "images"), target)
				if err := os.Remove(filepath.Join(target, proofName)); err != nil {
					t.Fatalf("remove owned dangling-target proof file: %v", err)
				}
				if err := os.Remove(target); err != nil {
					t.Fatalf("remove owned empty junction target: %v", err)
				}
				assertPageImagePathRejected(t, pagesDir, base, "images/secret.png")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, t.TempDir())
		})
	}
}

func TestPageImageWindowsPhysicalPath(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, root string)
	}{
		{
			name: "regular-file",
			run: func(t *testing.T, root string) {
				target := filepath.Join(root, "regular.png")
				writePageImageFixture(t, target, "regular-physical-image")
				assertPageImagePhysicalIdentity(t, target, target, "regular-physical-image")
			},
		},
		{
			name: "directory",
			run: func(t *testing.T, root string) {
				target := filepath.Join(root, "directory")
				if err := os.MkdirAll(target, 0755); err != nil {
					t.Fatalf("create directory target: %v", err)
				}
				assertPageImagePhysicalIdentity(t, target, target, "")
			},
		},
		{
			name: "spaces-unicode",
			run: func(t *testing.T, root string) {
				target := filepath.Join(root, "space üñîçødé", "image.png")
				writePageImageFixture(t, target, "unicode-physical-image")
				assertPageImagePhysicalIdentity(t, target, target, "unicode-physical-image")
			},
		},
		{
			name: "long-path",
			run: func(t *testing.T, root string) {
				path := root
				for _, component := range []string{
					strings.Repeat("a", 80),
					strings.Repeat("b", 80),
					strings.Repeat("c", 80),
					strings.Repeat("d", 80),
				} {
					path = filepath.Join(path, component)
				}
				target := filepath.Join(path, "long-image.png")
				extendedTarget := toExtendedWindowsPath(target)
				if length := len(utf16.Encode([]rune(extendedTarget))); length <= pageImageInitialPathBuffer {
					t.Fatalf("fixture path has %d UTF-16 units, want more than initial %d-unit buffer", length, pageImageInitialPathBuffer)
				}
				if err := os.MkdirAll(toExtendedWindowsPath(path), 0755); err != nil {
					t.Fatalf("create long-path directories: %v", err)
				}
				if err := os.WriteFile(extendedTarget, []byte("long-path-physical-image"), 0644); err != nil {
					t.Fatalf("write long-path image: %v", err)
				}
				resolved, err := resolvePageImagePhysicalPath(extendedTarget)
				if err != nil {
					t.Fatalf("resolve long-path image: %v", err)
				}
				if length := len(utf16.Encode([]rune(resolved))); length <= pageImageInitialPathBuffer {
					t.Fatalf("resolved long path has %d UTF-16 units, want more than initial %d-unit buffer", length, pageImageInitialPathBuffer)
				}
				assertPageImagePhysicalIdentity(t, extendedTarget, extendedTarget, "long-path-physical-image")
			},
		},
		{
			name: "missing",
			run: func(t *testing.T, root string) {
				if got, err := resolvePageImagePhysicalPath(filepath.Join(root, "missing.png")); err == nil {
					t.Fatalf("missing path unexpectedly resolved to %q", got)
				}
			},
		},
		{
			name: "invalid-nul",
			run: func(t *testing.T, root string) {
				if got, err := resolvePageImagePhysicalPath(filepath.Join(root, "invalid\x00.png")); err == nil {
					t.Fatalf("NUL-containing path unexpectedly resolved to %q", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, t.TempDir()) })
	}
}

func TestPageImageWindowsPathBoundary(t *testing.T) {
	tests := []struct {
		name string
		path string
		base string
		want bool
	}{
		{
			name: "descendant-dos",
			path: `\\?\C:\pages\guide\images\logo.png`,
			base: `\\?\C:\pages\guide\images`,
			want: true,
		},
		{
			name: "same-base-dos",
			path: `\\?\C:\pages\guide\images`,
			base: `\\?\C:\pages\guide\images`,
			want: false,
		},
		{
			name: "sibling-prefix-dos",
			path: `\\?\C:\pages\guide\images-private\secret.png`,
			base: `\\?\C:\pages\guide\images`,
			want: false,
		},
		{
			name: "other-volume",
			path: `\\?\D:\pages\guide\images\logo.png`,
			base: `\\?\C:\pages\guide\images`,
			want: false,
		},
		{
			name: "descendant-unc",
			path: `\\?\UNC\server\share\pages\guide\images\logo.png`,
			base: `\\?\UNC\server\share\pages\guide\images`,
			want: true,
		},
		{
			name: "other-share",
			path: `\\?\UNC\server\other-share\pages\guide\images\logo.png`,
			base: `\\?\UNC\server\share\pages\guide\images`,
			want: false,
		},
		{
			name: "case-dos",
			path: `\\?\c:\PAGES\Guide\Images\logo.png`,
			base: `\\?\C:\pages\guide\images`,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPathWithinBase(tt.path, tt.base); got != tt.want {
				t.Fatalf("isPathWithinBase(%q, %q) = %v, want %v", tt.path, tt.base, got, tt.want)
			}
		})
	}
}

func createPageImageWindowsJunction(t *testing.T, fixtureRoot, link, target string) string {
	t.Helper()
	fixtureAbs, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatalf("make junction fixture root absolute: %v", err)
	}
	linkAbs, err := filepath.Abs(link)
	if err != nil {
		t.Fatalf("make junction path absolute: %v", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		t.Fatalf("make junction target absolute: %v", err)
	}
	if !isPathWithinBase(linkAbs, fixtureAbs) || !isPathWithinBase(targetAbs, fixtureAbs) {
		t.Fatalf("junction link and target must be owned by fixture root %q: link=%q target=%q", fixtureAbs, linkAbs, targetAbs)
	}
	if err := os.MkdirAll(filepath.Dir(linkAbs), 0755); err != nil {
		t.Fatalf("create junction parent: %v", err)
	}
	if err := os.MkdirAll(targetAbs, 0755); err != nil {
		t.Fatalf("create junction target: %v", err)
	}
	proofName := ".junction-proof-" + filepath.Base(fixtureAbs) + ".txt"
	proofBody := []byte("windows-junction-proof:" + filepath.Base(fixtureAbs))
	if err := os.WriteFile(filepath.Join(targetAbs, proofName), proofBody, 0644); err != nil {
		t.Fatalf("write junction proof: %v", err)
	}
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", linkAbs, targetAbs).CombinedOutput()
	if err != nil {
		t.Fatalf("create real directory junction %q -> %q: %v: %s", linkAbs, targetAbs, err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(linkAbs); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove owned directory junction %q: %v", linkAbs, err)
		}
	})

	linkInfo, err := os.Stat(linkAbs)
	if err != nil {
		t.Fatalf("stat created junction %q: %v", linkAbs, err)
	}
	targetInfo, err := os.Stat(targetAbs)
	if err != nil {
		t.Fatalf("stat junction target %q: %v", targetAbs, err)
	}
	if !os.SameFile(linkInfo, targetInfo) {
		t.Fatalf("junction and target are not the same directory: link=%q target=%q", linkAbs, targetAbs)
	}
	gotProof, err := os.ReadFile(filepath.Join(linkAbs, proofName))
	if err != nil {
		t.Fatalf("read unique proof through junction %q: %v", linkAbs, err)
	}
	if !bytes.Equal(gotProof, proofBody) {
		t.Fatalf("junction proof body = %q, want %q", gotProof, proofBody)
	}
	return proofName
}

func assertPageImagePathRejected(t *testing.T, pagesDir, imagesDir, filename string) {
	t.Helper()
	if got, ok := resolvePageImagePath(pagesDir, imagesDir, filename); ok {
		t.Fatalf("expected physical path to be rejected, got %q", got)
	}
}

func assertPageImagePhysicalIdentity(t *testing.T, path, expected, body string) {
	t.Helper()
	resolved, err := resolvePageImagePhysicalPath(path)
	if err != nil {
		t.Fatalf("resolve physical path %q: %v", path, err)
	}
	if !isExtendedWindowsDOSPath(resolved) && !isExtendedWindowsUNCPath(resolved) {
		t.Fatalf("physical path %q is not in extended DOS/UNC form", resolved)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("stat resolved physical path %q: %v", resolved, err)
	}
	expectedInfo, err := os.Stat(expected)
	if err != nil {
		t.Fatalf("stat expected physical path %q: %v", expected, err)
	}
	if !os.SameFile(resolvedInfo, expectedInfo) {
		t.Fatalf("resolved physical path %q is not expected target %q", resolved, expected)
	}
	if body != "" {
		gotBody, err := os.ReadFile(resolved)
		if err != nil {
			t.Fatalf("read resolved physical file %q: %v", resolved, err)
		}
		if !bytes.Equal(gotBody, []byte(body)) {
			t.Fatalf("resolved physical body = %q, want %q", gotBody, body)
		}
	}
}

func toExtendedWindowsPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if isExtendedWindowsDOSPath(absolute) || isExtendedWindowsUNCPath(absolute) {
		return absolute
	}
	return pageImageExtendedPrefix + absolute
}
