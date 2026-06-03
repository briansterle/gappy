package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

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
	if err := crane.Push(img, dest, crane.Insecure); err != nil {
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
