package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestChart writes a minimal valid Helm .tgz into dir and returns its path.
func makeTestChart(t *testing.T, dir, name, version string) string {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	chartYaml := fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\ndescription: test chart\n", name, version)
	if err := tw.WriteHeader(&tar.Header{
		Name: name + "/Chart.yaml",
		Size: int64(len(chartYaml)),
		Mode: 0644,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(chartYaml)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	path := filepath.Join(dir, fmt.Sprintf("%s-%s.tgz", name, version))
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- resolveHelmAlias ---

func TestResolveHelmAlias(t *testing.T) {
	repos := []HelmRepository{
		{Name: "rs-dev-helm", URL: "https://artifactory.example.com/helm/rs-dev-helm"},
		{Name: "rs-dev-helm-oci", URL: "oci://artifactory.example.com/rs-dev-helm-oci"},
	}

	t.Run("resolves bare name", func(t *testing.T) {
		r := resolveHelmAlias("rs-dev-helm", repos)
		if r == nil || r.URL != repos[0].URL {
			t.Fatalf("got %v, want %s", r, repos[0].URL)
		}
	})

	t.Run("resolves @ prefix", func(t *testing.T) {
		r := resolveHelmAlias("@rs-dev-helm", repos)
		if r == nil || r.URL != repos[0].URL {
			t.Fatalf("got %v, want %s", r, repos[0].URL)
		}
	})

	t.Run("resolves OCI repo", func(t *testing.T) {
		r := resolveHelmAlias("@rs-dev-helm-oci", repos)
		if r == nil || r.URL != repos[1].URL {
			t.Fatalf("got %v, want %s", r, repos[1].URL)
		}
	})

	t.Run("returns nil for unknown alias", func(t *testing.T) {
		if r := resolveHelmAlias("nonexistent", repos); r != nil {
			t.Fatalf("expected nil, got %v", r)
		}
	})
}

// --- loadChartRefsFromHaulerWithRepos ---

func TestLoadChartRefsFromHaulerWithRepos(t *testing.T) {
	repos := []HelmRepository{
		{Name: "rs-dev-helm", URL: "https://artifactory.example.com/helm/rs-dev-helm"},
		{Name: "rs-dev-helm-oci", URL: "oci://artifactory.example.com/rs-dev-helm-oci"},
	}

	manifest := HaulerChartManifest{Kind: "Charts"}
	manifest.Spec.Charts = []struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
		RepoURL string `yaml:"repoURL"`
	}{
		{Name: "activemq", Version: "6.1.6", RepoURL: "rs-dev-helm"},
		{Name: "cert-manager", Version: "v1.14.0", RepoURL: "rs-dev-helm-oci"},
		{Name: "ghost-chart", Version: "1.0.0", RepoURL: "nonexistent"},
	}

	refs := loadChartRefsFromHaulerWithRepos(manifest, repos)

	t.Run("skips unknown repo", func(t *testing.T) {
		if len(refs) != 2 {
			t.Fatalf("expected 2 refs (nonexistent skipped), got %d", len(refs))
		}
	})

	t.Run("HTTP repo produces http ChartRef", func(t *testing.T) {
		r := refs[0]
		if r.Type != "http" {
			t.Errorf("expected http, got %s", r.Type)
		}
		if r.Name != "activemq" || r.Version != "6.1.6" || r.RepoName != "rs-dev-helm" {
			t.Errorf("unexpected ref: %+v", r)
		}
		if r.RepoURL != repos[0].URL {
			t.Errorf("unexpected RepoURL: %s", r.RepoURL)
		}
	})

	t.Run("OCI repo produces oci ChartRef", func(t *testing.T) {
		r := refs[1]
		if r.Type != "oci" {
			t.Errorf("expected oci, got %s", r.Type)
		}
		if !strings.Contains(r.OciRef, "cert-manager:v1.14.0") {
			t.Errorf("unexpected OciRef: %s", r.OciRef)
		}
		if strings.HasPrefix(r.OciRef, "oci://") {
			t.Errorf("OciRef should not have oci:// prefix, got: %s", r.OciRef)
		}
	})
}

// --- helmRepoHandler ---

func TestHelmRepoHandler(t *testing.T) {
	dir := t.TempDir()
	makeTestChart(t, dir, "activemq", "6.1.6")
	makeTestChart(t, dir, "redis", "7.0.0")

	h := &helmRepoHandler{dir: dir}

	t.Run("GET index.yaml returns 200 with both charts", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/index.yaml", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/x-yaml" {
			t.Errorf("expected application/x-yaml, got %s", ct)
		}
		body := w.Body.String()
		if !strings.Contains(body, "activemq") {
			t.Error("index.yaml missing activemq")
		}
		if !strings.Contains(body, "redis") {
			t.Error("index.yaml missing redis")
		}
	})

	t.Run("GET existing chart returns 200 with content", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/activemq-6.1.6.tgz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if w.Body.Len() == 0 {
			t.Error("expected non-empty body")
		}
	})

	t.Run("GET chart with wrong version returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/activemq-0.0.0.tgz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})

	t.Run("GET nonexistent chart returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/does-not-exist-1.0.0.tgz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})

	t.Run("GET non-tgz path returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/garbage", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
	})

	t.Run("empty repo serves empty index", func(t *testing.T) {
		emptyDir := t.TempDir()
		emptyHandler := &helmRepoHandler{dir: emptyDir}
		req := httptest.NewRequest(http.MethodGet, "/index.yaml", nil)
		w := httptest.NewRecorder()
		emptyHandler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "apiVersion: v1") {
			t.Error("expected valid index.yaml even for empty repo")
		}
	})
}
