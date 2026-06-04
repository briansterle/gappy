package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// startUpstream spins up an in-memory OCI registry to act as the upstream that
// gappy pulls from. Returns its host:port.
func startUpstream(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(registry.New())
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://")
}

// newTestServer builds a server backed by a temp store, forced anonymous auth
// (so the host's docker keychain is never consulted) and an ephemeral registry
// bind address.
func newTestServer(t *testing.T) *server {
	t.Helper()
	return &server{
		storeDir:    t.TempDir(),
		regBindAddr: "127.0.0.1:0",
		remoteAuth:  remote.WithAuth(authn.Anonymous),
		hub:         newHub(),
	}
}

func pushImage(t *testing.T, host, repoTag string, img v1.Image) string {
	t.Helper()
	ref, err := name.ParseReference(host+"/"+repoTag, name.Insecure)
	if err != nil {
		t.Fatalf("parse %q: %v", repoTag, err)
	}
	if err := remote.Write(ref, img, remote.WithAuth(authn.Anonymous)); err != nil {
		t.Fatalf("push %q: %v", repoTag, err)
	}
	return ref.Name()
}

func pushIndex(t *testing.T, host, repoTag string, idx v1.ImageIndex) string {
	t.Helper()
	ref, err := name.ParseReference(host+"/"+repoTag, name.Insecure)
	if err != nil {
		t.Fatalf("parse %q: %v", repoTag, err)
	}
	if err := remote.WriteIndex(ref, idx, remote.WithAuth(authn.Anonymous)); err != nil {
		t.Fatalf("push index %q: %v", repoTag, err)
	}
	return ref.Name()
}

// eventsOf returns the decoded payloads of every SSE frame of the given type.
func eventsOf(j *job, name string) []map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []map[string]any
	for _, f := range j.frames {
		if f.event == name {
			var m map[string]any
			if err := json.Unmarshal([]byte(f.data), &m); err == nil {
				out = append(out, m)
			}
		}
	}
	return out
}

func lastEvent(j *job, name string) map[string]any {
	evs := eventsOf(j, name)
	if len(evs) == 0 {
		return nil
	}
	return evs[len(evs)-1]
}

// seedStore writes an image directly into the store with a clean ref annotation.
func seedStore(t *testing.T, dir, ref string, img v1.Image) {
	t.Helper()
	lyt, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if err := lyt.WriteImage(img); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}
	d, err := partial.Descriptor(img)
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[refAnnotationKey] = ref
	if err := lyt.AppendDescriptor(*d); err != nil {
		t.Fatalf("AppendDescriptor: %v", err)
	}
}

// ── unit tests ────────────────────────────────────────────────────────────────

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{5 * 1024 * 1024 * 1024, "5.0 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEnumerateStoreEmpty(t *testing.T) {
	imgs, err := enumerateStore(t.TempDir())
	if err != nil {
		t.Fatalf("enumerateStore: %v", err)
	}
	if len(imgs) != 0 {
		t.Fatalf("expected 0 images, got %d", len(imgs))
	}
}

func TestEnumerateCharts(t *testing.T) {
	dir := t.TempDir()

	// no helm dir yet → empty, no error
	if cs, err := enumerateCharts(dir); err != nil || len(cs) != 0 {
		t.Fatalf("empty store: got %d charts, err %v", len(cs), err)
	}

	repoDir := filepath.Join(dir, "helm", "my-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	makeTestChart(t, repoDir, "redis", "1.2.3")
	makeTestChart(t, repoDir, "nginx", "4.5.6")

	charts, err := enumerateCharts(dir)
	if err != nil {
		t.Fatalf("enumerateCharts: %v", err)
	}
	if len(charts) != 2 {
		t.Fatalf("expected 2 charts, got %d", len(charts))
	}
	// sorted by repo then name → nginx before redis
	if charts[0].Name != "nginx" || charts[0].Version != "4.5.6" {
		t.Errorf("chart[0] = %+v, want nginx 4.5.6", charts[0])
	}
	if charts[1].Name != "redis" || charts[1].Repo != "my-repo" || charts[1].Size == 0 {
		t.Errorf("chart[1] = %+v, want redis in my-repo with size", charts[1])
	}
	if charts[0].Description != "test chart" {
		t.Errorf("description = %q, want %q", charts[0].Description, "test chart")
	}
}

func TestIsCached(t *testing.T) {
	dir := t.TempDir()
	img, err := random.Image(2048, 2)
	if err != nil {
		t.Fatal(err)
	}
	seedStore(t, dir, "cache/test:v1", img)

	layers, _ := img.Layers()
	d, _ := layers[0].Digest()
	sz, _ := layers[0].Size()
	if !isCached(dir, d.String(), sz) {
		t.Errorf("expected layer %s to be cached", d)
	}
	if isCached(dir, d.String(), sz+1) {
		t.Errorf("wrong-size blob should not count as cached")
	}
	if isCached(dir, "sha256:deadbeef", 10) {
		t.Errorf("absent blob should not be cached")
	}
}

// ── integration: pack ─────────────────────────────────────────────────────────

func TestPackImage(t *testing.T) {
	host := startUpstream(t)
	img, err := random.Image(4096, 3)
	if err != nil {
		t.Fatal(err)
	}
	ref := pushImage(t, host, "team/app:v1", img)

	s := newTestServer(t)
	j := s.hub.create("pack")
	s.runPack(j, ref) // synchronous

	if errs := eventsOf(j, "error"); len(errs) > 0 {
		t.Fatalf("pack errored: %v", errs[0]["msg"])
	}
	jobEv := lastEvent(j, "job")
	if jobEv == nil {
		t.Fatal("no job event emitted")
	}
	if got := jobEv["isIndex"].(bool); got {
		t.Errorf("single image should not be an index")
	}
	if got := len(jobEv["layers"].([]any)); got != 3 {
		t.Errorf("expected 3 layers in job event, got %d", got)
	}
	done := lastEvent(j, "done")
	if done == nil || done["ok"] != true {
		t.Fatalf("expected ok done event, got %v", done)
	}
	if len(eventsOf(j, "progress")) == 0 {
		t.Errorf("expected at least one progress event")
	}

	imgs, err := enumerateStore(s.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 {
		t.Fatalf("expected 1 image in store, got %d", len(imgs))
	}
	if imgs[0].Ref != ref {
		t.Errorf("ref = %q, want %q", imgs[0].Ref, ref)
	}
	if imgs[0].LayerCount != 3 {
		t.Errorf("layerCount = %d, want 3", imgs[0].LayerCount)
	}
	if imgs[0].IsIndex {
		t.Errorf("should not be flagged as index")
	}
}

func TestPackIndex(t *testing.T) {
	host := startUpstream(t)
	idx, err := random.Index(2048, 2, 3) // 3 child images, 2 layers each
	if err != nil {
		t.Fatal(err)
	}
	ref := pushIndex(t, host, "team/multi:v1", idx)

	s := newTestServer(t)
	j := s.hub.create("pack")
	s.runPack(j, ref)

	if errs := eventsOf(j, "error"); len(errs) > 0 {
		t.Fatalf("pack errored: %v", errs[0]["msg"])
	}
	jobEv := lastEvent(j, "job")
	if jobEv == nil || jobEv["isIndex"] != true {
		t.Fatalf("expected index job event, got %v", jobEv)
	}
	if lastEvent(j, "done")["ok"] != true {
		t.Fatal("expected ok done event")
	}

	imgs, err := enumerateStore(s.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || !imgs[0].IsIndex {
		t.Fatalf("expected 1 index in store, got %+v", imgs)
	}
	if imgs[0].LayerCount == 0 {
		t.Errorf("index should report layers across platforms")
	}
}

func TestPackIdempotentReplace(t *testing.T) {
	host := startUpstream(t)
	img, _ := random.Image(1024, 2)
	ref := pushImage(t, host, "team/app:v1", img)

	s := newTestServer(t)
	for i := 0; i < 2; i++ {
		j := s.hub.create("pack")
		s.runPack(j, ref)
		if errs := eventsOf(j, "error"); len(errs) > 0 {
			t.Fatalf("pack %d errored: %v", i, errs[0]["msg"])
		}
	}
	imgs, _ := enumerateStore(s.storeDir)
	if len(imgs) != 1 {
		t.Fatalf("re-packing same ref should not duplicate; got %d entries", len(imgs))
	}

	// Second pack should see all layers already cached.
	j := s.hub.create("pack")
	s.runPack(j, ref)
	jobEv := lastEvent(j, "job")
	if jobEv["cached"] != true {
		t.Errorf("expected cached=true on repeat pack, got %v", jobEv["cached"])
	}
}

// ── integration: unpack / serve ────────────────────────────────────────────────

func TestUnpackServesStore(t *testing.T) {
	s := newTestServer(t)
	img, err := random.Image(4096, 2)
	if err != nil {
		t.Fatal(err)
	}
	seedStore(t, s.storeDir, "team/app:v1", img)

	j := s.hub.create("unpack")
	s.runUnpack(j)

	if errs := eventsOf(j, "error"); len(errs) > 0 {
		t.Fatalf("unpack errored: %v", errs[0]["msg"])
	}
	done := lastEvent(j, "done")
	if done == nil || done["ok"] != true {
		t.Fatalf("expected ok done event, got %v", done)
	}
	if served := eventsOf(j, "served"); len(served) != 1 {
		t.Errorf("expected 1 served event, got %d", len(served))
	}

	// The store should now be pullable from the in-process registry.
	pullRef, err := name.ParseReference(s.regAddr+"/team/app:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Image(pullRef)
	if err != nil {
		t.Fatalf("pull back from registry: %v", err)
	}
	gotD, _ := got.Digest()
	wantD, _ := img.Digest()
	if gotD != wantD {
		t.Errorf("served digest %s != original %s", gotD, wantD)
	}
}

// ── integration: HTTP surface ──────────────────────────────────────────────────

func TestHTTPHandlers(t *testing.T) {
	host := startUpstream(t)
	img, _ := random.Image(4096, 2)
	ref := pushImage(t, host, "team/app:v1", img)

	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	client := &http.Client{Timeout: 30 * time.Second}

	// empty store
	if body := getJSON(t, client, ts.URL+"/api/images"); int(body["count"].(float64)) != 0 {
		t.Errorf("expected empty store, got count=%v", body["count"])
	}

	// validation
	if code := postCode(t, client, ts.URL+"/api/pack", `{"ref":""}`); code != http.StatusBadRequest {
		t.Errorf("empty ref: got %d, want 400", code)
	}
	if code := postCode(t, client, ts.URL+"/api/pack", `{"ref":"@@bad@@"}`); code != http.StatusBadRequest {
		t.Errorf("bad ref: got %d, want 400", code)
	}

	// stream for unknown job → 404
	resp, err := client.Get(ts.URL + "/api/stream?job=nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown job stream: got %d, want 404", resp.StatusCode)
	}

	// real pack via HTTP
	jobID := postJobID(t, client, ts.URL+"/api/pack", `{"ref":"`+ref+`"}`)
	if jobID == "" {
		t.Fatal("no jobId returned")
	}
	sse := readAll(t, client, ts.URL+"/api/stream?job="+jobID)
	if !strings.Contains(sse, "event: done") {
		t.Fatalf("stream did not complete; got:\n%s", sse)
	}
	if strings.Contains(sse, "event: error") {
		t.Fatalf("stream reported an error:\n%s", sse)
	}

	// store now has the image
	body := getJSON(t, client, ts.URL+"/api/images")
	if int(body["count"].(float64)) != 1 {
		t.Errorf("expected 1 image after pack, got %v", body["count"])
	}
}

// ── small HTTP helpers ──────────────────────────────────────────────────────────

func getJSON(t *testing.T, c *http.Client, url string) map[string]any {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return m
}

func postCode(t *testing.T, c *http.Client, url, body string) int {
	t.Helper()
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func postJobID(t *testing.T, c *http.Client, url, body string) string {
	t.Helper()
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	if id, ok := m["jobId"].(string); ok {
		return id
	}
	return ""
}

func readAll(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
