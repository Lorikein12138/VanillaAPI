package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestCleanPageImageRelativePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "single filename", in: "logo.png", want: "logo.png", ok: true},
		{name: "nested path", in: "images/logo.png", want: filepath.Join("images", "logo.png"), ok: true},
		{name: "dot prefix", in: "./logo.png", want: "logo.png", ok: true},
		{name: "url escaped slash", in: "images%2Flogo.png", want: filepath.Join("images", "logo.png"), ok: true},
		{name: "parent traversal", in: "../secret.png", ok: false},
		{name: "encoded parent traversal", in: "%2e%2e/secret.png", ok: false},
		{name: "backslash traversal", in: `images\secret.png`, ok: false},
		{name: "absolute path", in: "/etc/passwd", ok: false},
		{name: "encoded absolute path", in: "%2fetc/passwd", ok: false},
		{name: "encoded nul byte", in: "logo.png%00", ok: false},
		{name: "invalid escape", in: "logo.png%zz", ok: false},
		{name: "empty path", in: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cleanPageImageRelativePath(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolvePageImagePath(t *testing.T) {
	root := t.TempDir()
	pagesDir := filepath.Join(root, "pages")
	base := filepath.Join(pagesDir, "guide")
	if err := os.MkdirAll(filepath.Join(base, "images"), 0755); err != nil {
		t.Fatalf("create images dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "logo.png"), []byte("fake"), 0644); err != nil {
		t.Fatalf("create direct image: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "images", "logo.png"), []byte("fake"), 0644); err != nil {
		t.Fatalf("create image: %v", err)
	}

	got, ok := resolvePageImagePath(pagesDir, base, "logo.png")
	if !ok {
		t.Fatal("expected direct image path to be accepted")
	}
	assertResolvedPageImageMatchesFile(t, got, filepath.Join(base, "logo.png"))

	got, ok = resolvePageImagePath(pagesDir, base, "images/logo.png")
	if !ok {
		t.Fatal("expected nested image path to be accepted")
	}
	assertResolvedPageImageMatchesFile(t, got, filepath.Join(base, "images", "logo.png"))

	if got, ok := resolvePageImagePath(pagesDir, base, "../guide.md"); ok {
		t.Fatalf("expected traversal to be rejected, got %q", got)
	}
}

func TestResolvePageImagePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	pagesDir := filepath.Join(root, "pages")
	base := filepath.Join(pagesDir, "guide")
	outside := filepath.Join(root, "outside")

	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatalf("create page dir: %v", err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("create outside dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.png"), []byte("secret"), 0644); err != nil {
		t.Fatalf("create outside file: %v", err)
	}
	link := filepath.Join(base, "images")
	createPageImageEscapeLink(t, root, link, outside)

	if got, ok := resolvePageImagePath(pagesDir, base, "images/secret.png"); ok {
		t.Fatalf("expected symlink escape to be rejected, got %q", got)
	}
}

func createPageImageEscapeLink(t *testing.T, fixtureRoot, link, target string) {
	t.Helper()
	fixtureAbs, err := filepath.Abs(fixtureRoot)
	if err != nil {
		t.Fatalf("make fixture root absolute: %v", err)
	}
	linkAbs, err := filepath.Abs(link)
	if err != nil {
		t.Fatalf("make link path absolute: %v", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		t.Fatalf("make link target absolute: %v", err)
	}
	if !isPathWithinBase(linkAbs, fixtureAbs) || !isPathWithinBase(targetAbs, fixtureAbs) {
		t.Fatalf("link and target must be owned by fixture root %q: link=%q target=%q", fixtureAbs, linkAbs, targetAbs)
	}
	proofName := ".outside-link-proof-" + filepath.Base(fixtureAbs) + ".txt"
	proofBody := []byte("outside-link-proof:" + filepath.Base(fixtureAbs))
	if err := os.WriteFile(filepath.Join(targetAbs, proofName), proofBody, 0644); err != nil {
		t.Fatalf("write unique outside-link proof: %v", err)
	}

	if err := os.Symlink(targetAbs, linkAbs); err == nil {
		t.Cleanup(func() { _ = os.Remove(linkAbs) })
	} else if runtime.GOOS == "windows" {
		junction := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", linkAbs, targetAbs)
		output, junctionErr := junction.CombinedOutput()
		if junctionErr != nil {
			t.Fatalf("create directory junction %q -> %q: %v: %s", linkAbs, targetAbs, junctionErr, output)
		}
		t.Cleanup(func() { _ = os.Remove(linkAbs) })
	} else {
		t.Fatalf("create outside symlink %q -> %q: %v", linkAbs, targetAbs, err)
	}

	linkInfo, err := os.Stat(linkAbs)
	if err != nil {
		t.Fatalf("stat created link %q: %v", linkAbs, err)
	}
	targetInfo, err := os.Stat(targetAbs)
	if err != nil {
		t.Fatalf("stat outside target %q: %v", targetAbs, err)
	}
	if !os.SameFile(linkInfo, targetInfo) {
		t.Fatalf("created link and outside target are not the same directory: link=%q target=%q", linkAbs, targetAbs)
	}
	gotProof, err := os.ReadFile(filepath.Join(linkAbs, proofName))
	if err != nil {
		t.Fatalf("read unique proof through link %q: %v", linkAbs, err)
	}
	if !bytes.Equal(gotProof, proofBody) {
		t.Fatalf("link proof body = %q, want %q", gotProof, proofBody)
	}
}

func assertResolvedPageImageMatchesFile(t *testing.T, resolved, expected string) {
	t.Helper()
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("stat resolved image %q: %v", resolved, err)
	}
	expectedInfo, err := os.Stat(expected)
	if err != nil {
		t.Fatalf("stat expected image %q: %v", expected, err)
	}
	if !os.SameFile(resolvedInfo, expectedInfo) {
		t.Fatalf("resolved image %q is not expected file %q", resolved, expected)
	}
	resolvedBody, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read resolved image %q: %v", resolved, err)
	}
	expectedBody, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read expected image %q: %v", expected, err)
	}
	if !bytes.Equal(resolvedBody, expectedBody) {
		t.Fatalf("resolved image body = %q, want %q", resolvedBody, expectedBody)
	}
}

func TestServePageImagePathBoundary(t *testing.T) {
	previousGinMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previousGinMode) })
	const outsideBody = "outside-image-private-sentinel"
	tests := []struct {
		name       string
		visibility string
		filename   string
		wantStatus int
		wantBody   string
		setup      func(t *testing.T, root, base string)
	}{
		{
			name:       "regular",
			visibility: "public",
			filename:   "images/regular.png",
			wantStatus: http.StatusOK,
			wantBody:   "regular-image-body",
			setup: func(t *testing.T, _, base string) {
				writePageImageFixture(t, filepath.Join(base, "images", "regular.png"), "regular-image-body")
			},
		},
		{
			name:       "nested",
			visibility: "public",
			filename:   "images/nested/nested.png",
			wantStatus: http.StatusOK,
			wantBody:   "nested-image-body",
			setup: func(t *testing.T, _, base string) {
				writePageImageFixture(t, filepath.Join(base, "images", "nested", "nested.png"), "nested-image-body")
			},
		},
		{
			name:       "outside-link",
			visibility: "public",
			filename:   "images/secret.png",
			wantStatus: http.StatusNotFound,
			setup: func(t *testing.T, root, base string) {
				outside := filepath.Join(root, "outside")
				if err := os.MkdirAll(outside, 0755); err != nil {
					t.Fatalf("create outside directory: %v", err)
				}
				writePageImageFixture(t, filepath.Join(outside, "secret.png"), outsideBody)
				createPageImageEscapeLink(t, root, filepath.Join(base, "images"), outside)
			},
		},
		{
			name:       "missing",
			visibility: "public",
			filename:   "images/missing.png",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "admin-only",
			visibility: "admin",
			filename:   "images/admin.png",
			wantStatus: http.StatusNotFound,
			setup: func(t *testing.T, _, base string) {
				writePageImageFixture(t, filepath.Join(base, "images", "admin.png"), "admin-only-image-body")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			base := filepath.Join(root, "pages", "guide")
			if err := os.MkdirAll(base, 0755); err != nil {
				t.Fatalf("create page directory: %v", err)
			}
			if tt.setup != nil {
				tt.setup(t, root, base)
			}
			menu, err := json.Marshal([]map[string]string{{"url": "md:guide", "page_slug": "guide", "visibility": tt.visibility}})
			if err != nil {
				t.Fatalf("marshal menu setting: %v", err)
			}
			settingService := service.NewSettingService(&pageImageSettingRepo{customMenuItems: string(menu)}, nil)
			handler := NewPageHandler(root, settingService)
			router := gin.New()
			router.GET("/api/v1/pages/:slug/images/*filename", handler.ServePageImage)

			if tt.name == "outside-link" {
				visibilityRecorder := httptest.NewRecorder()
				visibilityContext, _ := gin.CreateTestContext(visibilityRecorder)
				visibilityContext.Request = httptest.NewRequest(http.MethodGet, "/api/v1/pages/guide/images/secret.png", nil)
				if !handler.checkImageSlugVisibility(visibilityContext, "guide") {
					t.Fatal("outside-link request did not pass public page visibility")
				}
			}

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/pages/guide/images/"+tt.filename, nil)
			router.ServeHTTP(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantStatus == http.StatusOK && recorder.Body.String() != tt.wantBody {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), tt.wantBody)
			}
			if tt.wantStatus != http.StatusOK && bytes.Contains(recorder.Body.Bytes(), []byte(outsideBody)) {
				t.Fatalf("response leaked outside image body %q", outsideBody)
			}
		})
	}
}

func writePageImageFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create image directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write image fixture: %v", err)
	}
}

type pageImageSettingRepo struct {
	customMenuItems string
}

func (r *pageImageSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	return &service.Setting{Key: key, Value: r.customMenuItems}, nil
}

func (r *pageImageSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if key != service.SettingKeyCustomMenuItems {
		return "", service.ErrSettingNotFound
	}
	return r.customMenuItems, nil
}

func (r *pageImageSettingRepo) Set(_ context.Context, key, value string) error {
	if key == service.SettingKeyCustomMenuItems {
		r.customMenuItems = value
	}
	return nil
}

func (r *pageImageSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{service.SettingKeyCustomMenuItems: r.customMenuItems}, nil
}

func (r *pageImageSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }

func (r *pageImageSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{service.SettingKeyCustomMenuItems: r.customMenuItems}, nil
}

func (r *pageImageSettingRepo) Delete(_ context.Context, key string) error {
	if key == service.SettingKeyCustomMenuItems {
		r.customMenuItems = ""
	}
	return nil
}
