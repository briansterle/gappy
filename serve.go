package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"gopkg.in/yaml.v3"
)

// --- helm chart index ------------------------------------------------------

// ChartMeta holds the fields needed from Chart.yaml inside a .tgz.
type ChartMeta struct {
	APIVersion  string `yaml:"apiVersion"`
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
}

// HelmIndex is the Helm HTTP repo index.yaml structure.
type HelmIndex struct {
	APIVersion string                        `yaml:"apiVersion"`
	Generated  string                        `yaml:"generated"`
	Entries    map[string][]HelmChartVersion `yaml:"entries"`
}

type HelmChartVersion struct {
	APIVersion  string   `yaml:"apiVersion"`
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description,omitempty"`
	Digest      string   `yaml:"digest"`
	URLs        []string `yaml:"urls"`
	Created     string   `yaml:"created"`
}

// readChartYamlFromTgz extracts and parses the Chart.yaml from inside a .tgz file.
func readChartYamlFromTgz(path string) (*ChartMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) == "Chart.yaml" {
			var meta ChartMeta
			if err := yaml.NewDecoder(tr).Decode(&meta); err != nil {
				return nil, err
			}
			return &meta, nil
		}
	}
	return nil, fmt.Errorf("Chart.yaml not found in %s", path)
}

func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildHelmIndex scans dir for .tgz files and builds a Helm HTTP repo index.yaml.
func buildHelmIndex(dir string) ([]byte, error) {
	entries := map[string][]HelmChartVersion{}

	files, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	if err != nil {
		return nil, err
	}

	for _, f := range files {
		meta, err := readChartYamlFromTgz(f)
		if err != nil {
			log.Printf("skipping %s in index: %v", filepath.Base(f), err)
			continue
		}
		digest, err := sha256OfFile(f)
		if err != nil {
			log.Printf("skipping %s in index: %v", filepath.Base(f), err)
			continue
		}
		cv := HelmChartVersion{
			APIVersion:  meta.APIVersion,
			Name:        meta.Name,
			Version:     meta.Version,
			Description: meta.Description,
			Digest:      digest,
			URLs:        []string{filepath.Base(f)},
			Created:     time.Now().UTC().Format(time.RFC3339),
		}
		entries[meta.Name] = append(entries[meta.Name], cv)
	}

	idx := HelmIndex{
		APIVersion: "v1",
		Generated:  time.Now().UTC().Format(time.RFC3339),
		Entries:    entries,
	}
	return yaml.Marshal(idx)
}

func helmIndexEntryCount(dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	return len(files), err
}

// --- helm http repo --------------------------------------------------------

// helmRepoHandler serves a Helm HTTP repository from a local directory.
// It generates index.yaml on the fly from the .tgz files present.
type helmRepoHandler struct {
	dir string
}

func (h *helmRepoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case p == "index.yaml":
		idx, err := buildHelmIndex(h.dir)
		if err != nil {
			log.Printf("helm index error %s: %v", h.dir, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		count, _ := helmIndexEntryCount(h.dir)
		log.Printf("helm 200 GET index.yaml (%s, %d charts)", filepath.Base(h.dir), count)
		w.Header().Set("Content-Type", "application/x-yaml")
		w.Write(idx)

	case strings.HasSuffix(p, ".tgz"):
		name := filepath.Base(p)
		f, err := os.Open(filepath.Join(h.dir, name))
		if err != nil {
			log.Printf("helm 404 GET %s/%s", filepath.Base(h.dir), name)
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		log.Printf("helm 200 GET %s/%s", filepath.Base(h.dir), name)
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, f)

	default:
		log.Printf("helm 404 GET %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

// --- oci blob handler ------------------------------------------------------

// layoutBlobHandler serves blobs directly from an OCI layout
type layoutBlobHandler struct {
	blobsDir string
}

// blobNotFound bridges a missing blob to the registry's internal "not found"
// sentinel (an unexported errors.New("not found")), so HEAD/GET on an absent
// blob returns 404 instead of 500. The registry matches it with errors.Is,
// which consults this Is method. Docker push depends on this: the client HEADs
// every blob before uploading and treats anything but 404 as a fatal error.
type blobNotFound struct{ err error }

func (e blobNotFound) Error() string { return "not found: " + e.err.Error() }

func (e blobNotFound) Unwrap() error { return e.err }

func (blobNotFound) Is(target error) bool { return target != nil && target.Error() == "not found" }

func (h *layoutBlobHandler) Get(ctx context.Context, repo string, hash v1.Hash) (io.ReadCloser, error) {
	path := filepath.Join(h.blobsDir, hash.Algorithm, hash.Hex)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, blobNotFound{err}
	}
	return f, err
}

func (h *layoutBlobHandler) Stat(ctx context.Context, repo string, hash v1.Hash) (int64, error) {
	path := filepath.Join(h.blobsDir, hash.Algorithm, hash.Hex)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, blobNotFound{err}
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Put persists an uploaded blob into the content-addressed store so that
// `docker push localhost:5000/...` actually keeps its layers and config. The
// registry passes rc wrapped in a digest+size verifier, so io.Copy returns a
// verification error on mismatch, which the registry maps to the right response.
func (h *layoutBlobHandler) Put(ctx context.Context, repo string, hash v1.Hash, rc io.ReadCloser) error {
	defer rc.Close()
	dir := filepath.Join(h.blobsDir, hash.Algorithm)
	if err := perm.mkdirAll(dir); err != nil {
		return err
	}
	// Temp file in the same dir keeps the rename atomic and on one device.
	tmp, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	// os.CreateTemp is always 0600 whatever the umask, so a pushed blob has to
	// be chmod'ed explicitly or nobody else can read what was pushed in.
	perm.chmodFile(tmp.Name())
	return os.Rename(tmp.Name(), filepath.Join(dir, hash.Hex))
}

// --- push-through to the store ---------------------------------------------

// storeWriter wraps the registry handler so images pushed with
// `docker push localhost:5000/...` are persisted into the OCI layout and survive
// a serve restart, becoming part of the portable store. Pushed blobs are already
// written by layoutBlobHandler.Put; here we additionally write each pushed
// manifest blob into the layout and, for tag pushes, add (or replace) a
// top-level descriptor in index.json.
//
// ready gates persistence: cmdServe replays the existing store into the
// in-process registry on startup, and those internal pushes flow through this
// same handler. They are already in the store, so persistence stays off until
// the replay finishes and only genuine client pushes are recorded.
type storeWriter struct {
	inner http.Handler
	lyt   layout.Path
	ready atomic.Bool
	mu    sync.Mutex
}

func (s *storeWriter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	repo, ref, ok := parseManifestPush(r)
	if !ok || !s.ready.Load() {
		s.inner.ServeHTTP(w, r)
		return
	}

	// Buffer the manifest so we can persist it after the registry accepts it.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	contentType := r.Header.Get("Content-Type")

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.inner.ServeHTTP(rec, r)
	if rec.status < 200 || rec.status >= 300 {
		return
	}

	if err := s.persist(repo, ref, contentType, body); err != nil {
		log.Printf("pushed %s:%s but failed to persist into store: %v", repo, ref, err)
		return
	}
	log.Printf("persisted pushed %s:%s into store", repo, ref)
}

// persist writes the manifest blob into the layout and, for tag pushes, records
// a top-level descriptor in index.json. Child manifests pushed by digest are
// referenced by their parent index, so they only need their blob on disk.
func (s *storeWriter) persist(repo, ref, contentType string, body []byte) error {
	hash, _, err := v1.SHA256(bytes.NewReader(body))
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.lyt.WriteBlob(hash, io.NopCloser(bytes.NewReader(body))); err != nil {
		return err
	}
	perm.chmodFile(filepath.Join(string(s.lyt), "blobs", hash.Algorithm, hash.Hex))

	if _, err := v1.NewHash(ref); err == nil {
		return nil // pushed by digest — a child manifest, no index entry needed
	}

	refName := repo + ":" + ref
	desc := v1.Descriptor{
		MediaType:   types.MediaType(contentType),
		Size:        int64(len(body)),
		Digest:      hash,
		Annotations: map[string]string{refAnnotationKey: refName},
	}
	// Replace any existing descriptor for this tag so re-pushing updates in place.
	if err := s.lyt.RemoveDescriptors(match.Name(refName)); err != nil {
		return err
	}
	if err := s.lyt.AppendDescriptor(desc); err != nil {
		return err
	}
	perm.chmodFile(filepath.Join(string(s.lyt), "index.json"))
	return nil
}

// statusRecorder captures the response status so storeWriter only persists a
// manifest after the registry has accepted it.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// parseManifestPush reports whether r is a manifest PUT and returns the repo and
// tag/digest reference. Path form: /v2/{repo...}/manifests/{ref}.
func parseManifestPush(r *http.Request) (repo, ref string, ok bool) {
	if r.Method != http.MethodPut {
		return "", "", false
	}
	elem := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(elem) < 4 || elem[0] != "v2" || elem[len(elem)-2] != "manifests" {
		return "", "", false
	}
	return strings.Join(elem[1:len(elem)-2], "/"), elem[len(elem)-1], true
}

// --- serve -----------------------------------------------------------------

func cmdServe(storePath string) {
	useStore(storePath)
	lyt, err := layout.FromPath(storePath)
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}
	idx, err := lyt.ImageIndex()
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}

	blobsDir := fmt.Sprintf("%s/blobs", storePath)
	log.Printf("serving blobs explicitly from %s", blobsDir)

	handler := &layoutBlobHandler{blobsDir: blobsDir}

	reg := registry.New(
		registry.WithReferrersSupport(false),
		registry.WithBlobHandler(handler),
	)

	// storeWriter persists `docker push` uploads into the layout. It stays in
	// pass-through mode until the startup replay below finishes (see ready).
	sw := &storeWriter{inner: reg, lyt: lyt}

	mux := http.NewServeMux()
	mux.Handle("/v2/", sw)

	// Register one Helm HTTP repo handler per subdirectory of ./store/helm/.
	// Each repo is served at /{repoName}/ so clients can point their helm repo
	// alias at http://localhost:5000/{repoName}.
	helmBase := filepath.Join(storePath, "helm")
	if entries, err := os.ReadDir(helmBase); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			repoName := e.Name()
			dir := filepath.Join(helmBase, repoName)
			prefix := "/" + repoName + "/"
			mux.Handle(prefix, http.StripPrefix(prefix, &helmRepoHandler{dir: dir}))
			log.Printf("serving Helm repo %s at %s", repoName, prefix)
		}
	}

	addr := "127.0.0.1:5000"
	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Fatal(server.ListenAndServe())
	}()

	time.Sleep(100 * time.Millisecond)

	log.Printf("loading store into registry...")
	_, failed, err := pushStore(idx, addr, []name.Option{name.Insecure})
	if err != nil {
		log.Fatal(err)
	}
	if len(failed) > 0 {
		log.Printf("%d image(s) failed to load: %s", len(failed), strings.Join(failed, ", "))
	}

	// Replay finished: start recording genuine client pushes into the store.
	sw.ready.Store(true)
	log.Printf("registry ready on %s — accepting docker push", addr)

	select {}
}

// --- push to a registry ----------------------------------------------------

// pushStore writes every ref-named entry of idx to prefix/<ref>, jobs at a
// time. It returns how many landed and which refs failed. Multi-arch indexes
// go over whole, and blobs the registry already holds are skipped.
func pushStore(idx v1.ImageIndex, prefix string, nameOpts []name.Option, opts ...remote.Option) (int, []string, error) {
	im, err := idx.IndexManifest()
	if err != nil {
		return 0, nil, err
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		pushed int
		failed []string
	)
	sem := make(chan struct{}, jobs)

	for _, d := range im.Manifests {
		ref := d.Annotations[refAnnotationKey]
		if ref == "" {
			log.Printf("skipping manifest with no ref annotation: %s", d.Digest)
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			dest := prefix + "/" + ref
			err := pushDescriptor(idx, d, dest, nameOpts, opts)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("push failed %s: %v", ref, err)
				failed = append(failed, ref)
				return
			}
			log.Printf("pushed %s → %s", ref, dest)
			pushed++
		}()
	}

	wg.Wait()
	slices.Sort(failed)
	return pushed, failed, nil
}

// pushDescriptor writes the image or index d points at to dest.
func pushDescriptor(idx v1.ImageIndex, d v1.Descriptor, dest string, nameOpts []name.Option, opts []remote.Option) error {
	destRef, err := name.ParseReference(dest, nameOpts...)
	if err != nil {
		return err
	}
	switch d.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		child, err := idx.ImageIndex(d.Digest)
		if err != nil {
			return err
		}
		return remote.WriteIndex(destRef, child, opts...)
	default:
		img, err := idx.Image(d.Digest)
		if err != nil {
			return err
		}
		return remote.Write(destRef, img, opts...)
	}
}

// cmdPush copies every image in the store to a remote registry. A ref that
// serve exposes as localhost:5000/<ref> lands at <prefix>/<ref>, so
// artifactory.company.org/dev-docker turns nginx:alpine into
// artifactory.company.org/dev-docker/nginx:alpine.
func cmdPush(prefix, storePath string, dryRun bool) {
	var nameOpts []name.Option
	if strings.HasPrefix(prefix, "http://") {
		nameOpts = append(nameOpts, name.Insecure)
	}
	prefix = strings.TrimPrefix(prefix, "http://")
	prefix = strings.TrimPrefix(prefix, "https://")
	prefix = strings.TrimSuffix(prefix, "/")
	if _, err := name.NewRepository(prefix, nameOpts...); err != nil {
		log.Fatalf("bad registry prefix %q: %v", prefix, err)
	}

	idx, err := layout.ImageIndexFromPath(storePath)
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}

	if dryRun {
		im, err := idx.IndexManifest()
		if err != nil {
			log.Fatal(err)
		}
		for _, d := range im.Manifests {
			if ref := d.Annotations[refAnnotationKey]; ref != "" {
				fmt.Printf("  %s → %s/%s\n", ref, prefix, ref)
			}
		}
		return
	}

	pushed, failed, err := pushStore(idx, prefix, nameOpts, makeRemoteAuthOption())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\npushed %d image(s) to %s\n", pushed, prefix)
	if len(failed) > 0 {
		fmt.Printf("failed %d:\n", len(failed))
		for _, ref := range failed {
			fmt.Printf("  %s\n", ref)
		}
		os.Exit(1)
	}
}
