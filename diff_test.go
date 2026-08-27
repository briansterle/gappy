package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiffTextImages(t *testing.T) {
	dir := t.TempDir()

	baseFile := filepath.Join(dir, "base-images.txt")
	currFile := filepath.Join(dir, "curr-images.txt")
	outFile := filepath.Join(dir, "diff-images.txt")

	baseContent := "nginx:alpine\nredis:alpine\nubuntu:24.04\n"
	currContent := "nginx:alpine\nredis:alpine\npostgres:17-alpine\npython:3.13-alpine\n"

	writeFile(t, baseFile, baseContent)
	writeFile(t, currFile, currContent)

	cmdDiff(baseFile, currFile, outFile)

	outStr := string(readFile(t, outFile))
	if strings.Contains(outStr, "nginx:alpine") || strings.Contains(outStr, "redis:alpine") {
		t.Errorf("expected common images to be removed, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "postgres:17-alpine") || !strings.Contains(outStr, "python:3.13-alpine") {
		t.Errorf("expected delta images to be present, got:\n%s", outStr)
	}
}

func TestDiffFoundCharts(t *testing.T) {
	dir := t.TempDir()

	baseFile := filepath.Join(dir, "base-charts.txt")
	currFile := filepath.Join(dir, "curr-charts.txt")
	outFile := filepath.Join(dir, "diff-charts.txt")

	baseContent := "http::my-repo::https://example.com/helm::my-chart::1.0.0\noci::registry.example.com/charts/my-oci:2.0.0\n"
	currContent := "http::my-repo::https://example.com/helm::my-chart::1.0.0\nhttp::my-repo::https://example.com/helm::new-chart::2.1.0\noci::registry.example.com/charts/my-oci:2.0.0\noci::registry.example.com/charts/new-oci:3.0.0\n"

	writeFile(t, baseFile, baseContent)
	writeFile(t, currFile, currContent)

	cmdDiff(baseFile, currFile, outFile)

	outStr := string(readFile(t, outFile))
	if strings.Contains(outStr, "my-chart::1.0.0") || strings.Contains(outStr, "my-oci:2.0.0") {
		t.Errorf("expected baseline charts to be removed, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "new-chart::2.1.0") || !strings.Contains(outStr, "new-oci:3.0.0") {
		t.Errorf("expected new charts to be present, got:\n%s", outStr)
	}
}

func TestDiffFromZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")

	// Create baseline zip
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create("resolute-installer/sidecar-charts/found-images.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("ubuntu:24.04\nnginx:alpine\n"))
	zw.Close()
	zf.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "found-images.diff.txt")
	writeFile(t, currFile, "ubuntu:24.04\nnginx:alpine\nredis:alpine\n")

	cmdDiff(zipPath, currFile, outFile)

	outStr := strings.TrimSpace(string(readFile(t, outFile)))
	if outStr != "redis:alpine" {
		t.Errorf("got %q, want redis:alpine", outStr)
	}
}

// --- Hauler manifest diffing ---

func TestDiffHaulerImages(t *testing.T) {
	dir := t.TempDir()

	baseFile := filepath.Join(dir, "base-images.txt")
	currFile := filepath.Join(dir, "images-manifest.yaml")
	outFile := filepath.Join(dir, "delta.yaml")

	writeFile(t, baseFile, "nginx:alpine\nredis:alpine\n")
	writeFile(t, currFile, `apiVersion: content.hauler.cattle.io/v1alpha1
kind: Images
spec:
  images:
    - name: nginx:alpine
    - name: postgres:17-alpine
    - name: redis:alpine
      rewrite: mirror.example.com/redis:alpine
`)

	cmdDiff(baseFile, currFile, outFile)

	var got HaulerManifest
	if err := yaml.Unmarshal(readFile(t, outFile), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Images) != 1 {
		t.Fatalf("expected 1 image, got %d: %+v", len(got.Spec.Images), got.Spec.Images)
	}
	if got.Spec.Images[0].Name != "postgres:17-alpine" {
		t.Errorf("got %q, want postgres:17-alpine", got.Spec.Images[0].Name)
	}
}

func TestDiffHaulerCharts(t *testing.T) {
	dir := t.TempDir()

	baseFile := filepath.Join(dir, "found-charts.txt")
	currFile := filepath.Join(dir, "charts-manifest.yaml")
	outFile := filepath.Join(dir, "delta.yaml")

	// The baseline spells the chart as a found-charts.txt line; the current
	// manifest spells the same chart as Hauler YAML. They must still match.
	writeFile(t, baseFile, "http::my-repo::https://example.com/helm::my-chart::1.0.0\n")
	writeFile(t, currFile, `apiVersion: content.hauler.cattle.io/v1alpha1
kind: Charts
spec:
  charts:
    - name: my-chart
      version: 1.0.0
      repoURL: https://example.com/helm
    - name: new-chart
      version: 2.1.0
      repoURL: https://example.com/helm
`)

	cmdDiff(baseFile, currFile, outFile)

	var got HaulerChartManifest
	if err := yaml.Unmarshal(readFile(t, outFile), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Charts) != 1 {
		t.Fatalf("expected 1 chart, got %d: %+v", len(got.Spec.Charts), got.Spec.Charts)
	}
	if got.Spec.Charts[0].Name != "new-chart" {
		t.Errorf("got %q, want new-chart", got.Spec.Charts[0].Name)
	}
}

// TestDiffBaselineIsHaulerChartManifest runs the pairing the other way round:
// the baseline is Hauler YAML and the current manifest is found-charts.txt.
func TestDiffBaselineIsHaulerChartManifest(t *testing.T) {
	dir := t.TempDir()
	baseFile := filepath.Join(dir, "charts-manifest.yaml")
	currFile := filepath.Join(dir, "found-charts.txt")
	outFile := filepath.Join(dir, "delta.txt")

	writeFile(t, baseFile, `apiVersion: content.hauler.cattle.io/v1alpha1
kind: Charts
spec:
  charts:
    - name: my-chart
      version: 1.0.0
      repoURL: https://example.com/helm
`)
	writeFile(t, currFile, "http::my-repo::https://example.com/helm::my-chart::1.0.0\nhttp::my-repo::https://example.com/helm::new-chart::2.0.0\n")

	cmdDiff(baseFile, currFile, outFile)

	got := strings.TrimSpace(string(readFile(t, outFile)))
	want := "http::my-repo::https://example.com/helm::new-chart::2.0.0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- directory baselines ---

func TestDiffFromDirWithStoreIndex(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	if err := os.MkdirAll(store, 0755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(store, "index.json"), `{
	  "schemaVersion": 2,
	  "manifests": [
	    {"mediaType":"application/vnd.oci.image.manifest.v1+json",
	     "digest":"sha256:`+strings.Repeat("a", 64)+`",
	     "size":1,
	     "annotations":{"org.opencontainers.image.ref.name":"nginx:alpine"}}
	  ]
	}`)

	chartDir := filepath.Join(store, "helm")
	if err := os.MkdirAll(chartDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(chartDir, "my-chart-1.0.0.tgz"), "not really a chart")

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "nginx:alpine\nredis:alpine\nhttp::r::https://example.com::my-chart::1.0.0\n")

	cmdDiff(store, currFile, outFile)

	got := strings.TrimSpace(string(readFile(t, outFile)))
	if got != "redis:alpine" {
		t.Errorf("got %q, want redis:alpine", got)
	}
}

// TestDiffIgnoresUnrelatedYAMLLines pins the rule that a non-Hauler YAML in the
// baseline tree contributes no keys. Injecting its raw lines would let a stray
// image ref in some values.yaml mask a real image and drop it from the delta.
func TestDiffIgnoresUnrelatedYAMLLines(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "values.yaml"), "replicas: 2\nnginx:alpine\n")

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "nginx:alpine\n")

	cmdDiff(base, currFile, outFile)

	got := strings.TrimSpace(string(readFile(t, outFile)))
	if got != "nginx:alpine" {
		t.Errorf("unrelated YAML masked a real image: got %q, want nginx:alpine", got)
	}
}

// --- output shape ---

func TestDiffPreservesCommentsAndCountsOnlyRefs(t *testing.T) {
	dir := t.TempDir()
	baseFile := filepath.Join(dir, "base-images.txt")
	currFile := filepath.Join(dir, "curr-images.txt")
	outFile := filepath.Join(dir, "delta.txt")

	writeFile(t, baseFile, "nginx:alpine\n")
	writeFile(t, currFile, "# infra\nnginx:alpine\n\n# apps\nredis:alpine\n")

	cmdDiff(baseFile, currFile, outFile)

	got := string(readFile(t, outFile))
	want := "# infra\n\n# apps\nredis:alpine\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDiffEmptyBaselineKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	baseFile := filepath.Join(dir, "empty.txt")
	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")

	writeFile(t, baseFile, "")
	writeFile(t, currFile, "nginx:alpine\nredis:alpine\n")

	cmdDiff(baseFile, currFile, outFile)

	got := string(readFile(t, outFile))
	if got != "nginx:alpine\nredis:alpine\n" {
		t.Errorf("empty baseline must keep every entry, got %q", got)
	}
}

func TestCollectBaselineKeysMissingPathErrors(t *testing.T) {
	if _, err := collectBaselineKeys(filepath.Join(t.TempDir(), "nope.zip")); err == nil {
		t.Fatal("expected an error for a missing baseline")
	}
}

// TestDiffFromZipStoreEntries covers the zip paths that aren't plain manifests:
// an OCI index.json descriptor and a packaged chart under helm/.
func TestDiffFromZipStoreEntries(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")

	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for name, content := range map[string]string{
		"bundle/store/index.json": `{"schemaVersion":2,"manifests":[{"digest":"sha256:` +
			strings.Repeat("b", 64) + `","size":1,"annotations":{"org.opencontainers.image.ref.name":"nginx:alpine"}}]}`,
		"bundle/helm/my-chart-1.0.0.tgz": "chart bytes",
		"bundle/README.md":               "nginx:alpine is not a key here",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zf.Close()

	currFile := filepath.Join(dir, "found-charts.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "http::r::https://example.com::my-chart::1.0.0\nhttp::r::https://example.com::new-chart::2.0.0\n")

	cmdDiff(zipPath, currFile, outFile)

	got := strings.TrimSpace(string(readFile(t, outFile)))
	want := "http::r::https://example.com::new-chart::2.0.0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- tar baselines ---

func TestDiffFromTarGz(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "baseline.tar.gz")
	writeTarGz(t, tarPath, map[string]string{
		"bundle/found-images.txt": "ubuntu:24.04\nnginx:alpine\n",
	})

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "ubuntu:24.04\nnginx:alpine\nredis:alpine\n")

	cmdDiff(tarPath, currFile, outFile)

	if got := strings.TrimSpace(string(readFile(t, outFile))); got != "redis:alpine" {
		t.Errorf("got %q, want redis:alpine", got)
	}
}

func TestDiffFromUncompressedTar(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "baseline.tar")
	writeTar(t, tarPath, map[string]string{"bundle/images.txt": "nginx:alpine\n"}, false)

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "nginx:alpine\nredis:alpine\n")

	cmdDiff(tarPath, currFile, outFile)

	if got := strings.TrimSpace(string(readFile(t, outFile))); got != "redis:alpine" {
		t.Errorf("got %q, want redis:alpine", got)
	}
}

// TestTarAndZipAgree pins the property that made the two walkers worth sharing:
// the same bundle keyed through either format yields the same key set.
func TestTarAndZipAgree(t *testing.T) {
	dir := t.TempDir()
	members := map[string]string{
		"bundle/store/index.json": `{"schemaVersion":2,"manifests":[{"digest":"sha256:` +
			strings.Repeat("c", 64) + `","size":1,"annotations":{"org.opencontainers.image.ref.name":"nginx:alpine"}}]}`,
		"bundle/helm/my-chart-1.0.0.tgz": "chart bytes",
		"bundle/found-images.txt":        "redis:alpine\n",
		"bundle/README.md":               "postgres:17 is not a key",
	}

	tarPath := filepath.Join(dir, "b.tar.gz")
	zipPath := filepath.Join(dir, "b.zip")
	writeTarGz(t, tarPath, members)
	writeZip(t, zipPath, members)

	fromTar, err := collectBaselineKeys(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	fromZip, err := collectBaselineKeys(zipPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(fromTar) != len(fromZip) {
		t.Fatalf("tar keys %v != zip keys %v", fromTar, fromZip)
	}
	for k := range fromZip {
		if !fromTar[k] {
			t.Errorf("key %q present for zip but not tar", k)
		}
	}
	for _, want := range []string{"nginx:alpine", "redis:alpine", "my-chart-1.0.0.tgz"} {
		if !fromZip[want] {
			t.Errorf("missing key %q", want)
		}
	}
	if fromZip["postgres:17 is not a key"] {
		t.Error("README lines must not become keys")
	}
}

// --- remote baselines ---

func TestDiffFromHTTPZipRanged(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")
	writeZip(t, zipPath, map[string]string{"bundle/found-images.txt": "ubuntu:24.04\nnginx:alpine\n"})

	var ranged bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranged = true
		}
		http.ServeFile(w, r, zipPath)
	}))
	defer ts.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "ubuntu:24.04\nnginx:alpine\npostgres:17-alpine\n")

	cmdDiff(ts.URL+"/baseline.zip", currFile, outFile)

	if got := strings.TrimSpace(string(readFile(t, outFile))); got != "postgres:17-alpine" {
		t.Errorf("got %q, want postgres:17-alpine", got)
	}
	if !ranged {
		t.Error("expected the remote zip to be read with range requests")
	}
}

// TestDiffFromHTTPZipNoRangeSupport covers a server that declines ranges up
// front. The probe sees Accept-Ranges: none and downloads the archive whole,
// so the baseline is still read rather than the diff failing.
func TestDiffFromHTTPZipNoRangeSupport(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")
	writeZip(t, zipPath, map[string]string{"bundle/found-images.txt": "ubuntu:24.04\nnginx:alpine\n"})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore Range entirely, the way a naive proxy does.
		w.Header().Set("Accept-Ranges", "none")
		data, err := os.ReadFile(zipPath)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Length", itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer ts.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "ubuntu:24.04\nnginx:alpine\npostgres:17-alpine\n")

	cmdDiff(ts.URL+"/baseline.zip", currFile, outFile)

	if got := strings.TrimSpace(string(readFile(t, outFile))); got != "postgres:17-alpine" {
		t.Errorf("got %q, want postgres:17-alpine", got)
	}
}

// TestDiffFromHTTPZipLyingRangeServer covers a server that advertises
// Accept-Ranges: bytes and then ignores the Range header. The probe believes
// it, so the guard has to be in the read itself: without it archive/zip is fed
// the head of the file under every offset, and the diff fails outright.
func TestDiffFromHTTPZipLyingRangeServer(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")
	writeZip(t, zipPath, map[string]string{"bundle/found-images.txt": "ubuntu:24.04\nnginx:alpine\n"})

	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			w.Write(data)
		}
	}))
	defer ts.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "ubuntu:24.04\nnginx:alpine\npostgres:17-alpine\n")

	cmdDiff(ts.URL+"/baseline.zip", currFile, outFile)

	if got := strings.TrimSpace(string(readFile(t, outFile))); got != "postgres:17-alpine" {
		t.Errorf("got %q, want postgres:17-alpine", got)
	}
}

func TestDiffFromHTTPTarGz(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "baseline.tar.gz")
	writeTarGz(t, tarPath, map[string]string{"bundle/found-images.txt": "ubuntu:24.04\nnginx:alpine\n"})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, tarPath)
	}))
	defer ts.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "delta.txt")
	writeFile(t, currFile, "ubuntu:24.04\nnginx:alpine\ngolang:1.24-alpine\n")

	cmdDiff(ts.URL+"/baseline.tar.gz", currFile, outFile)

	if got := strings.TrimSpace(string(readFile(t, outFile))); got != "golang:1.24-alpine" {
		t.Errorf("got %q, want golang:1.24-alpine", got)
	}
}

func TestRemoteBaselineSendsCredentials(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")
	writeZip(t, zipPath, map[string]string{"bundle/found-images.txt": "nginx:alpine\n"})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "svc" || pass != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.ServeFile(w, r, zipPath)
	}))
	defer ts.Close()

	t.Setenv("GAPPY_USER", "svc")
	t.Setenv("GAPPY_PASS", "s3cret")

	keys, err := collectBaselineKeys(ts.URL + "/baseline.zip")
	if err != nil {
		t.Fatal(err)
	}
	if !keys["nginx:alpine"] {
		t.Errorf("expected nginx:alpine in %v", keys)
	}
}

// TestRemoteBaselineTokenOnlyAuth covers an Artifactory identity token, which
// is sent as the password with no username.
func TestRemoteBaselineTokenOnlyAuth(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")
	writeZip(t, zipPath, map[string]string{"bundle/found-images.txt": "nginx:alpine\n"})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); !ok || pass != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.ServeFile(w, r, zipPath)
	}))
	defer ts.Close()

	t.Setenv("GAPPY_USER", "")
	t.Setenv("GAPPY_PASS", "tok")

	keys, err := collectBaselineKeys(ts.URL + "/baseline.zip")
	if err != nil {
		t.Fatal(err)
	}
	if !keys["nginx:alpine"] {
		t.Errorf("expected nginx:alpine in %v", keys)
	}
}

func TestRemoteBaselineUnauthorizedErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	t.Setenv("GAPPY_USER", "")
	t.Setenv("GAPPY_PASS", "")
	t.Setenv("RSART_LOCAL_USER", "")
	t.Setenv("RSART_LOCAL_AUTH", "")

	_, err := collectBaselineKeys(ts.URL + "/baseline.zip")
	if err == nil {
		t.Fatal("expected an error for 401")
	}
	if !strings.Contains(err.Error(), "GAPPY_USER") {
		t.Errorf("error should name the auth env vars, got: %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://svc:s3cret@art.example.com/b.zip", "https://art.example.com/b.zip"},
		{"https://art.example.com/b.zip?token=abc", "https://art.example.com/b.zip?..."},
		{"https://art.example.com/b.zip", "https://art.example.com/b.zip"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := redactURL(u); got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCredsFromURLUserinfoWins(t *testing.T) {
	t.Setenv("GAPPY_USER", "env-user")
	t.Setenv("GAPPY_PASS", "env-pass")

	u, err := url.Parse("https://url-user:url-pass@art.example.com/b.zip")
	if err != nil {
		t.Fatal(err)
	}
	if user, pass := credsFor(u); user != "url-user" || pass != "url-pass" {
		t.Errorf("got %q/%q, want url-user/url-pass", user, pass)
	}
}

func TestSizeFromContentRange(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"bytes 0-0/12345", 12345},
		{"bytes 0-0/*", 0},
		{"", 0},
		{"garbage", 0},
	} {
		if got := sizeFromContentRange(tc.in); got != tc.want {
			t.Errorf("sizeFromContentRange(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// --- helpers ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func itoa(n int) string { return strconv.Itoa(n) }

func writeZip(t *testing.T, path string, members map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, content := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTarGz(t *testing.T, path string, members map[string]string) {
	t.Helper()
	writeTar(t, path, members, true)
}

func writeTar(t *testing.T, path string, members map[string]string, gzipped bool) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var w io.WriteCloser = nopWriteCloser{f}
	if gzipped {
		w = gzip.NewWriter(f)
	}
	tw := tar.NewWriter(w)
	for name, content := range members {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Size:     int64(len(content)),
			Mode:     0644,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
