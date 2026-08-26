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
	"runtime/debug"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// --- manifests, discovery, and chart index ---------------------------------

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
		{Name: "my-helm-repo", URL: "https://charts.example.com/helm/my-helm-repo"},
		{Name: "my-helm-repo-oci", URL: "oci://charts.example.com/my-helm-repo-oci"},
	}

	t.Run("resolves bare name", func(t *testing.T) {
		r := resolveHelmAlias("my-helm-repo", repos)
		if r == nil || r.URL != repos[0].URL {
			t.Fatalf("got %v, want %s", r, repos[0].URL)
		}
	})

	t.Run("resolves @ prefix", func(t *testing.T) {
		r := resolveHelmAlias("@my-helm-repo", repos)
		if r == nil || r.URL != repos[0].URL {
			t.Fatalf("got %v, want %s", r, repos[0].URL)
		}
	})

	t.Run("resolves OCI repo", func(t *testing.T) {
		r := resolveHelmAlias("@my-helm-repo-oci", repos)
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
		{Name: "my-helm-repo", URL: "https://charts.example.com/helm/my-helm-repo"},
		{Name: "my-helm-repo-oci", URL: "oci://charts.example.com/my-helm-repo-oci"},
	}

	manifest := HaulerChartManifest{Kind: "Charts"}
	manifest.Spec.Charts = []struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
		RepoURL string `yaml:"repoURL"`
	}{
		{Name: "my-chart", Version: "6.1.6", RepoURL: "my-helm-repo"},
		{Name: "my-oci-chart", Version: "v1.14.0", RepoURL: "my-helm-repo-oci"},
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
		if r.Name != "my-chart" || r.Version != "6.1.6" || r.RepoName != "my-helm-repo" {
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
		if !strings.Contains(r.OciRef, "my-oci-chart:v1.14.0") {
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
	makeTestChart(t, dir, "my-chart", "6.1.6")
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
		if !strings.Contains(body, "my-chart") {
			t.Error("index.yaml missing my-chart")
		}
		if !strings.Contains(body, "redis") {
			t.Error("index.yaml missing redis")
		}
	})

	t.Run("GET existing chart returns 200 with content", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/my-chart-6.1.6.tgz", nil)
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
		req := httptest.NewRequest(http.MethodGet, "/my-chart-0.0.0.tgz", nil)
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

// --- push-through registry -------------------------------------------------

// newPushTestRegistry spins up an empty store served by storeWriter (with
// persistence enabled) and returns the layout and the registry host:port.
func newPushTestRegistry(t *testing.T) (layout.Path, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := layout.Write(dir, empty.Index); err != nil {
		t.Fatal(err)
	}
	lyt, err := layout.FromPath(dir)
	if err != nil {
		t.Fatal(err)
	}

	reg := registry.New(
		registry.WithReferrersSupport(false),
		registry.WithBlobHandler(&layoutBlobHandler{blobsDir: dir + "/blobs"}),
	)
	sw := &storeWriter{inner: reg, lyt: lyt}
	sw.ready.Store(true)

	srv := httptest.NewServer(sw)
	t.Cleanup(srv.Close)

	return lyt, strings.TrimPrefix(srv.URL, "http://")
}

// descriptorForRef finds the index.json descriptor with the given ref name.
func descriptorForRef(t *testing.T, lyt layout.Path, ref string) *v1.Descriptor {
	t.Helper()
	idx, err := lyt.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for i := range im.Manifests {
		if im.Manifests[i].Annotations["org.opencontainers.image.ref.name"] == ref {
			return &im.Manifests[i]
		}
	}
	return nil
}

func TestPushImagePersistsToStore(t *testing.T) {
	lyt, host := newPushTestRegistry(t)

	img, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	dest := host + "/myapp:v1"
	if err := crane.Push(img, dest, crane.Insecure, crane.WithAuth(authn.Anonymous)); err != nil {
		t.Fatalf("push failed: %v", err)
	}

	// index.json should now carry the tagged image.
	desc := descriptorForRef(t, lyt, "myapp:v1")
	if desc == nil {
		t.Fatal("pushed image not found in index.json")
	}
	if desc.Digest != want {
		t.Fatalf("descriptor digest = %s, want %s", desc.Digest, want)
	}

	// Reloading the store from disk must yield the same image, blobs and all.
	reloaded, err := layout.ImageIndexFromPath(string(lyt))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Image(want)
	if err != nil {
		t.Fatalf("image missing after reload: %v", err)
	}
	gotDigest, err := got.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != want {
		t.Fatalf("reloaded digest = %s, want %s", gotDigest, want)
	}
	if _, err := got.RawConfigFile(); err != nil {
		t.Fatalf("config blob missing after reload: %v", err)
	}
}

func TestPushIndexPersistsChildBlobs(t *testing.T) {
	lyt, host := newPushTestRegistry(t)

	idx, err := random.Index(1024, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	want, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}

	ref, err := name.ParseReference(host+"/multi:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	// WriteIndex pushes the child manifests by digest first, then the index by
	// tag — the same sequence docker uses for a multi-arch push.
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("push index failed: %v", err)
	}

	// Only the tagged index gets a top-level descriptor; children are reachable
	// through it.
	if descriptorForRef(t, lyt, "multi:v1") == nil {
		t.Fatal("pushed index not found in index.json")
	}

	reloaded, err := layout.ImageIndexFromPath(string(lyt))
	if err != nil {
		t.Fatal(err)
	}
	child, err := reloaded.ImageIndex(want)
	if err != nil {
		t.Fatalf("index missing after reload: %v", err)
	}
	cm, err := child.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cm.Manifests {
		if _, err := child.Image(m.Digest); err != nil {
			t.Fatalf("child image %s missing after reload: %v", m.Digest, err)
		}
	}
}

// --- build info ------------------------------------------------------------

func TestBuildStampFallback(t *testing.T) {
	stamped := buildStamp{version: "v1.1.0", commit: "abc1234", date: "2026-08-26T00:00:00Z"}
	unstamped := buildStamp{version: "dev", commit: "none", date: "unknown"}

	t.Run("ldflags win over build info", func(t *testing.T) {
		got := stamped.withBuildInfo(&debug.BuildInfo{
			Main: debug.Module{Version: "v9.9.9"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "9999999999999999"},
				{Key: "vcs.time", Value: "2020-01-01T00:00:00Z"},
			},
		})
		if got != stamped {
			t.Errorf("build info overrode -ldflags: got %+v, want %+v", got, stamped)
		}
	})

	t.Run("go install records the module version", func(t *testing.T) {
		got := unstamped.withBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: "v1.1.0"}})
		if got.version != "v1.1.0" {
			t.Errorf("version: got %q, want v1.1.0", got.version)
		}
	})

	t.Run("(devel) is not a version", func(t *testing.T) {
		got := unstamped.withBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}})
		if got.version != "dev" {
			t.Errorf("version: got %q, want dev", got.version)
		}
	})

	t.Run("vcs settings fill commit and date", func(t *testing.T) {
		got := unstamped.withBuildInfo(&debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "fd1c915abcdef0123456"},
				{Key: "vcs.time", Value: "2026-08-01T02:06:38Z"},
			},
		})
		if got.commit != "fd1c915" {
			t.Errorf("commit: got %q, want fd1c915", got.commit)
		}
		if got.date != "2026-08-01T02:06:38Z" {
			t.Errorf("date: got %q, want the commit time", got.date)
		}
		if !got.dateFromCommit {
			t.Error("date came from vcs.time but dateFromCommit is false")
		}
	})

	t.Run("dirty tree is marked", func(t *testing.T) {
		got := unstamped.withBuildInfo(&debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "fd1c915abcdef0123456"},
				{Key: "vcs.modified", Value: "true"},
			},
		})
		if got.commit != "fd1c915-dirty" {
			t.Errorf("commit: got %q, want fd1c915-dirty", got.commit)
		}
	})

	t.Run("no build info at all", func(t *testing.T) {
		if got := unstamped.withBuildInfo(&debug.BuildInfo{}); got != unstamped {
			t.Errorf("got %+v, want defaults %+v", got, unstamped)
		}
	})
}

// --- permissions -----------------------------------------------------------

// withHostileUmask sets umask 077 — the setting that poisons a shared store —
// and restores the process umask and store policy afterwards.
func withHostileUmask(t *testing.T) {
	t.Helper()
	oldPerm := perm
	oldMask := setUmask(0o077)
	t.Cleanup(func() {
		setUmask(oldMask)
		perm = oldPerm
	})
}

func TestPermFromDir(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mode            os.FileMode
		wantDir         string
		wantFile        string
		wantUmask       int
		wantShutsOthers bool
	}{
		{"shared setgid", 0o775 | os.ModeSetgid, "2775", "0664", 0o002, false},
		{"world readable", 0o755, "0755", "0644", 0o022, false},
		{"private", 0o700, "0700", "0600", 0o077, true},
		{"group only", 0o750, "0750", "0640", 0o027, false},
		{"traverse only", 0o711, "0711", "0600", 0o066, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := permFromDir(tc.mode | os.ModeDir)
			if got := octal(p.dir); got != tc.wantDir {
				t.Errorf("dir: got %s, want %s", got, tc.wantDir)
			}
			if got := octal(p.file); got != tc.wantFile {
				t.Errorf("file: got %s, want %s", got, tc.wantFile)
			}
			if got := p.umask(); got != tc.wantUmask {
				t.Errorf("umask: got %#o, want %#o", got, tc.wantUmask)
			}
			if got := p.shutsOthersOut(); got != tc.wantShutsOthers {
				t.Errorf("shutsOthersOut: got %v, want %v", got, tc.wantShutsOthers)
			}
		})
	}
}

// The setgid bit must never survive onto a regular file, where it means
// something else entirely.
func TestPermFileDropsSetgid(t *testing.T) {
	p := permFromDir(0o2775 | os.ModeDir | os.ModeSetgid)
	if p.file&(os.ModeSetgid|os.ModeSetuid|os.ModeSticky) != 0 {
		t.Errorf("file mode carries a special bit: %s", octal(p.file))
	}
}

// useStore must set the process umask, because that is the only lever over
// go-containerregistry, which writes blobs with os.Create and index.json with
// os.ModePerm. Without it a umask-077 writer leaves a store nobody can read.
func TestUseStoreGovernsThirdPartyWrites(t *testing.T) {
	withHostileUmask(t)

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	useStore(dir)

	// Exactly what the layout package does.
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, os.ModePerm); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(blobs, "deadbeef"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	fi, err := os.Stat(filepath.Join(blobs, "deadbeef"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("blob written by a third-party os.Create: got %04o, want 0644", got)
	}
	di, err := os.Stat(blobs)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o755 {
		t.Errorf("dir written by a third-party MkdirAll: got %04o, want 0755", got)
	}
}

// Blobs gappy writes itself must land on the policy no matter the umask.
func TestStoreWritesIgnoreUmask(t *testing.T) {
	withHostileUmask(t)

	base := newTestStore(t)
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	inc := newTestStore(t)
	addTestImage(t, inc, "app:v1")

	// The increment is written 0600, as another user's store would be.
	incBlobs := filepath.Join(inc, "blobs", "sha256")
	entries, err := os.ReadDir(incBlobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(incBlobs, e.Name()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cmdMerge(base, []string{inc}, false)

	// Every blob and the index must be readable by group and other.
	merged, err := os.ReadDir(filepath.Join(base, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) == 0 {
		t.Fatal("merge copied no blobs")
	}
	for _, e := range merged {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("merged blob %s: got %04o, want 0644", e.Name()[:12], got)
		}
	}
	fi, err := os.Stat(filepath.Join(base, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("index.json: got %04o, want 0644", got)
	}
}

// os.CreateTemp is always 0600 regardless of umask, so the docker-push path
// has to chmod explicitly or nothing pushed in is readable by anyone else.
func TestPushedBlobsAreReadable(t *testing.T) {
	oldPerm := perm
	perm = defaultStorePerm
	t.Cleanup(func() { perm = oldPerm })

	lyt, host := newPushTestRegistry(t)
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, host+"/pushed:v1"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(string(lyt), "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("push wrote no blobs")
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("pushed blob %s: got %04o, want 0644 (os.CreateTemp leaves 0600)", e.Name()[:12], got)
		}
	}
}

// fix-perms brings an existing store in line with its directory's policy.
func TestFixPermsRepairsStore(t *testing.T) {
	oldPerm := perm
	t.Cleanup(func() { perm = oldPerm })

	dir := newTestStore(t)
	addTestImage(t, dir, "app:v1")

	blobs := filepath.Join(dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(blobs, e.Name()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "index.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	cmdFixPerms(dir, false)

	for _, e := range entries {
		fi, err := os.Stat(filepath.Join(blobs, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("blob %s: got %04o, want 0644", e.Name()[:12], got)
		}
	}
	for _, path := range []string{filepath.Join(dir, "index.json")} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o644 {
			t.Errorf("%s: got %04o, want 0644", filepath.Base(path), got)
		}
	}
	fi, err := os.Stat(blobs)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("blobs dir: got %04o, want 0755", got)
	}
}

// A dry run reports without touching anything.
func TestFixPermsDryRun(t *testing.T) {
	oldPerm := perm
	t.Cleanup(func() { perm = oldPerm })

	dir := newTestStore(t)
	addTestImage(t, dir, "app:v1")
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "index.json")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}

	cmdFixPerms(dir, true)

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("dry run changed the mode: got %04o, want 0600", got)
	}
}

// --- store merge -----------------------------------------------------------

// newTestStore creates an empty OCI store in a temp dir.
func newTestStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := layout.Write(dir, empty.Index); err != nil {
		t.Fatal(err)
	}
	return dir
}

// addTestImage writes a random image into the store under ref, replacing any
// existing entry with that ref (what cmdPack does).
func addTestImage(t *testing.T, dir, ref string) v1.Hash {
	t.Helper()
	lyt, err := layout.FromPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := lyt.WriteImage(img); err != nil {
		t.Fatal(err)
	}
	d, err := descriptorFor(img, ref)
	if err != nil {
		t.Fatal(err)
	}
	_ = lyt.RemoveDescriptors(func(e v1.Descriptor) bool {
		return e.Annotations[refAnnotationKey] == ref
	})
	if err := lyt.AppendDescriptor(d); err != nil {
		t.Fatal(err)
	}
	return d.Digest
}

// storeRefs maps ref name → digest for every entry in the store index.
func storeRefs(t *testing.T, dir string) map[string]v1.Hash {
	t.Helper()
	idx, err := readStoreIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]v1.Hash, len(idx.Manifests))
	for _, d := range idx.Manifests {
		out[d.Annotations[refAnnotationKey]] = d.Digest
	}
	return out
}

func blobCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

func TestMergeAddsNewImages(t *testing.T) {
	base := newTestStore(t)
	inc := newTestStore(t)
	keep := addTestImage(t, base, "nginx:alpine")
	added := addTestImage(t, inc, "redis:alpine")

	cmdMerge(base, []string{inc}, false)

	refs := storeRefs(t, base)
	if len(refs) != 2 {
		t.Fatalf("want 2 images, got %d: %v", len(refs), refs)
	}
	if refs["nginx:alpine"] != keep {
		t.Errorf("base image changed: got %s, want %s", refs["nginx:alpine"], keep)
	}
	if refs["redis:alpine"] != added {
		t.Errorf("merged image: got %s, want %s", refs["redis:alpine"], added)
	}

	// Every blob the merged entry references must have landed.
	reachable, err := loadReferencedBlobs(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	for hex := range reachable {
		if _, err := os.Stat(filepath.Join(base, "blobs", "sha256", hex)); err != nil {
			t.Errorf("blob %s missing after merge: %v", hex[:12], err)
		}
	}
}

func TestMergeDedupesSharedBlobs(t *testing.T) {
	base := newTestStore(t)
	inc := newTestStore(t)
	addTestImage(t, base, "nginx:alpine")

	// Copy the base store wholesale: every blob is already present, so the
	// merge should add nothing.
	for _, name := range []string{"index.json", "oci-layout"} {
		if err := blobCopy(filepath.Join(base, name), filepath.Join(inc, name)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(base, "blobs", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(inc, "blobs", "sha256"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := blobCopy(filepath.Join(base, "blobs", "sha256", e.Name()),
			filepath.Join(inc, "blobs", "sha256", e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	before := blobCount(t, base)
	cmdMerge(base, []string{inc}, false)

	if got := blobCount(t, base); got != before {
		t.Errorf("blob count changed on identical merge: %d → %d", before, got)
	}
	if refs := storeRefs(t, base); len(refs) != 1 {
		t.Errorf("want 1 image after identical merge, got %d: %v", len(refs), refs)
	}
}

func TestMergeUpdatedTagReplacesEntry(t *testing.T) {
	base := newTestStore(t)
	inc := newTestStore(t)
	old := addTestImage(t, base, "nginx:alpine")
	updated := addTestImage(t, inc, "nginx:alpine")
	if old == updated {
		t.Fatal("random images collided")
	}

	cmdMerge(base, []string{inc}, false)

	refs := storeRefs(t, base)
	if len(refs) != 1 {
		t.Fatalf("want 1 entry for the tag, got %d: %v", len(refs), refs)
	}
	if refs["nginx:alpine"] != updated {
		t.Errorf("tag not updated: got %s, want %s", refs["nginx:alpine"], updated)
	}
}

func TestMergeCharts(t *testing.T) {
	base := newTestStore(t)
	inc := newTestStore(t)

	baseRepo := filepath.Join(base, "helm", "my-repo")
	incRepo := filepath.Join(inc, "helm", "my-repo")
	for _, d := range []string{baseRepo, incRepo} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	makeTestChart(t, baseRepo, "shared", "1.0.0")
	makeTestChart(t, incRepo, "shared", "1.0.0")
	makeTestChart(t, incRepo, "brand-new", "2.0.0")

	cmdMerge(base, []string{inc}, false)

	for _, want := range []string{"shared-1.0.0.tgz", "brand-new-2.0.0.tgz"} {
		if _, err := os.Stat(filepath.Join(baseRepo, want)); err != nil {
			t.Errorf("chart %s missing after merge: %v", want, err)
		}
	}
}

func TestMergeDryRunWritesNothing(t *testing.T) {
	base := newTestStore(t)
	inc := newTestStore(t)
	addTestImage(t, base, "nginx:alpine")
	addTestImage(t, inc, "redis:alpine")

	beforeBlobs := blobCount(t, base)
	beforeIdx, err := os.ReadFile(filepath.Join(base, "index.json"))
	if err != nil {
		t.Fatal(err)
	}

	cmdMerge(base, []string{inc}, true)

	if got := blobCount(t, base); got != beforeBlobs {
		t.Errorf("dry run copied blobs: %d → %d", beforeBlobs, got)
	}
	afterIdx, err := os.ReadFile(filepath.Join(base, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterIdx) != string(beforeIdx) {
		t.Error("dry run rewrote index.json")
	}
}

func TestExtractDryRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
		rest []string
	}{
		{"no flag", []string{"store", "inc"}, false, []string{"store", "inc"}},
		{"short", []string{"-n", "store", "inc"}, true, []string{"store", "inc"}},
		{"long", []string{"store", "--dry-run", "inc"}, true, []string{"store", "inc"}},
		{"single dash long", []string{"store", "inc", "-dry-run"}, true, []string{"store", "inc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rest, dry := extractDryRun(tc.args)
			if dry != tc.want {
				t.Errorf("dryRun: got %v, want %v", dry, tc.want)
			}
			if len(rest) != len(tc.rest) {
				t.Fatalf("rest: got %v, want %v", rest, tc.rest)
			}
			for i := range rest {
				if rest[i] != tc.rest[i] {
					t.Errorf("rest[%d]: got %q, want %q", i, rest[i], tc.rest[i])
				}
			}
		})
	}
}
