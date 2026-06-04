package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"gopkg.in/yaml.v3"
)

var (
	jobs    int
	webAddr string
)

var (
	version = "v0.1.0"
	commit  = "none"
	date    = "unknown"
)

type HaulerManifest struct {
	Spec struct {
		Images []struct {
			Name    string `yaml:"name"`
			Rewrite string `yaml:"rewrite"`
		} `yaml:"images"`
	} `yaml:"spec"`
}

type HaulerChartManifest struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Charts []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
			RepoURL string `yaml:"repoURL"`
		} `yaml:"charts"`
	} `yaml:"spec"`
}

type ImageRef struct {
	Source  string
	Rewrite string
}

// ChartRef is a parsed entry from found-charts.txt.
type ChartRef struct {
	Type     string // "oci", "http", "local"
	RepoName string
	RepoURL  string
	Name     string
	Version  string
	OciRef   string
	FilePath string
}

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

func (e blobNotFound) Error() string      { return "not found: " + e.err.Error() }
func (e blobNotFound) Unwrap() error      { return e.err }
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	return os.Rename(tmp.Name(), filepath.Join(dir, hash.Hex))
}

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

	if _, err := v1.NewHash(ref); err == nil {
		return nil // pushed by digest — a child manifest, no index entry needed
	}

	refName := repo + ":" + ref
	desc := v1.Descriptor{
		MediaType:   types.MediaType(contentType),
		Size:        int64(len(body)),
		Digest:      hash,
		Annotations: map[string]string{"org.opencontainers.image.ref.name": refName},
	}
	// Replace any existing descriptor for this tag so re-pushing updates in place.
	if err := s.lyt.RemoveDescriptors(match.Name(refName)); err != nil {
		return err
	}
	return s.lyt.AppendDescriptor(desc)
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

func helmIndexEntryCount(dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	return len(files), err
}

func init() {
	flag.IntVar(&jobs, "j", max(1, runtime.NumCPU()-1), "parallel jobs")
	flag.StringVar(&webAddr, "web", "", "gappy web address to stream pack progress to (e.g. http://127.0.0.1:8080)")
}

func makeAuthOption() crane.Option {
	user := os.Getenv("GAPPY_USER")
	pass := os.Getenv("GAPPY_PASS")
	if user != "" && pass != "" {
		return crane.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username: user,
			Password: pass,
		}))
	}
	return crane.WithAuthFromKeychain(authn.DefaultKeychain)
}

func makeRemoteAuthOption() remote.Option {
	user := os.Getenv("GAPPY_USER")
	pass := os.Getenv("GAPPY_PASS")
	if user != "" && pass != "" {
		return remote.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username: user,
			Password: pass,
		}))
	}
	return remote.WithAuthFromKeychain(authn.DefaultKeychain)
}

func makeHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

func loadRefs(path string) ([]ImageRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		var manifest HaulerManifest
		if err := yaml.Unmarshal(data, &manifest); err == nil && len(manifest.Spec.Images) > 0 {
			var refs []ImageRef
			for _, img := range manifest.Spec.Images {
				if img.Name != "" {
					ref := ImageRef{Source: img.Name, Rewrite: img.Name}
					if img.Rewrite != "" {
						ref.Rewrite = img.Rewrite
					}
					refs = append(refs, ref)
				}
			}
			return refs, nil
		}
	}

	var refs []ImageRef
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			refs = append(refs, ImageRef{Source: line, Rewrite: line})
		}
	}
	return refs, scanner.Err()
}

func loadChartRefsFromHauler(manifest HaulerChartManifest) []ChartRef {
	return loadChartRefsFromHaulerWithRepos(manifest, loadHelmRepos())
}

func loadChartRefsFromHaulerWithRepos(manifest HaulerChartManifest, repos []HelmRepository) []ChartRef {
	var refs []ChartRef
	for _, chart := range manifest.Spec.Charts {
		r := resolveHelmAlias(chart.RepoURL, repos)
		if r == nil {
			log.Printf("skipping %s — repo %s not found in ~/.config/helm/repositories.yaml", chart.Name, chart.RepoURL)
			continue
		}
		if strings.HasPrefix(r.URL, "oci://") {
			base := strings.TrimPrefix(r.URL, "oci://")
			refs = append(refs, ChartRef{
				Type:   "oci",
				OciRef: fmt.Sprintf("%s/%s:%s", base, chart.Name, chart.Version),
			})
		} else {
			refs = append(refs, ChartRef{
				Type:     "http",
				RepoName: r.Name,
				RepoURL:  r.URL,
				Name:     chart.Name,
				Version:  chart.Version,
			})
		}
	}
	return refs
}

func loadChartRefs(path string) ([]ChartRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		var manifest HaulerChartManifest
		if err := yaml.Unmarshal(data, &manifest); err == nil && manifest.Kind == "Charts" && len(manifest.Spec.Charts) > 0 {
			return loadChartRefsFromHauler(manifest), nil
		}
	}

	var refs []ChartRef
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "::")
		switch parts[0] {
		case "oci":
			if len(parts) >= 2 {
				refs = append(refs, ChartRef{Type: "oci", OciRef: parts[1]})
			}
		case "http":
			if len(parts) >= 5 {
				refs = append(refs, ChartRef{
					Type:     "http",
					RepoName: parts[1],
					RepoURL:  parts[2],
					Name:     parts[3],
					Version:  parts[4],
				})
			}
		case "local":
			if len(parts) >= 3 {
				refs = append(refs, ChartRef{
					Type:     "local",
					RepoName: parts[1],
					FilePath: parts[2],
				})
			}
		}
	}
	return refs, scanner.Err()
}

func pullWithRetry(ref string, authOpt crane.Option, attempts int) (v1.Image, error) {
	var err error
	for i := range attempts {
		var img v1.Image
		img, err = crane.Pull(ref, authOpt)
		if err == nil {
			return img, nil
		}
		log.Printf("pull attempt %d/%d failed %s: %v", i+1, attempts, ref, err)
		time.Sleep(time.Duration(i+1) * 2 * time.Second)
	}
	return nil, err
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

// describable is the minimal surface shared by v1.Image and v1.ImageIndex
// needed to build the OCI layout descriptor for the store's index.json.
type describable interface {
	MediaType() (types.MediaType, error)
	Digest() (v1.Hash, error)
	Size() (int64, error)
}

func descriptorFor(d describable, ref string) (v1.Descriptor, error) {
	mt, err := d.MediaType()
	if err != nil {
		return v1.Descriptor{}, err
	}
	dig, err := d.Digest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	sz, err := d.Size()
	if err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{
		MediaType:   mt,
		Size:        sz,
		Digest:      dig,
		Annotations: map[string]string{"org.opencontainers.image.ref.name": ref},
	}, nil
}

// cliReporter streams pack progress from the CLI to a running gappy web server
// so the browser transit visualization works for CLI-initiated packs.
type cliReporter struct {
	addr   string
	jobID  string
	client *http.Client
}

func newCLIReporter(addr string) *cliReporter {
	if addr == "" {
		return nil
	}
	return &cliReporter{addr: strings.TrimRight(addr, "/"), client: &http.Client{Timeout: 5 * time.Second}}
}

// register sends the initial job metadata and returns the browser watch URL.
func (r *cliReporter) register(jobData map[string]any) (string, error) {
	b, err := json.Marshal(jobData)
	if err != nil {
		return "", err
	}
	resp, err := r.client.Post(r.addr+"/api/cli/start", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var res struct {
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if res.JobID == "" {
		return "", fmt.Errorf("empty jobId in response")
	}
	r.jobID = res.JobID
	return r.addr + "?job=" + res.JobID, nil
}

// push sends a single SSE frame to the web server for the active job.
func (r *cliReporter) push(event string, data any) {
	if r.jobID == "" {
		return
	}
	b, err := json.Marshal(map[string]any{"event": event, "data": data})
	if err != nil {
		return
	}
	resp, err := r.client.Post(r.addr+"/api/cli/event?job="+r.jobID, "application/json", bytes.NewReader(b))
	if err != nil {
		return
	}
	resp.Body.Close()
}

func cmdPack(manifestPath string, rep *cliReporter) {
	refs, err := loadRefs(manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("loaded %d image refs", len(refs))

	var lyt layout.Path
	if _, statErr := os.Stat("./store"); os.IsNotExist(statErr) {
		lyt, err = layout.Write("./store", empty.Index)
	} else {
		lyt, err = layout.FromPath("./store")
	}
	if err != nil {
		log.Fatal(err)
	}

	craneAuthOpt := makeAuthOption()
	remoteAuthOpt := makeRemoteAuthOption()

	// metaResult caches the manifest metadata gathered during the reporter
	// pre-flight phase so goroutines don't re-fetch it during blob download.
	type metaResult struct {
		desc    *remote.Descriptor
		seeds   []layerSeed
		isIndex bool
	}
	meta := make([]metaResult, len(refs))

	var sink *packSink
	var stopTicker chan struct{}
	var tickerDone <-chan struct{}

	if rep != nil {
		// Pre-flight: gather layer metadata concurrently to populate the job event
		// with accurate layer counts and total bytes before any blobs land.
		sem0 := make(chan struct{}, jobs)
		var wg0 sync.WaitGroup
		for i, ref := range refs {
			wg0.Add(1)
			sem0 <- struct{}{}
			go func(i int, ref ImageRef) {
				defer wg0.Done()
				defer func() { <-sem0 }()
				named, err := name.ParseReference(ref.Source)
				if err != nil {
					log.Printf("reporter: parse %s: %v", ref.Source, err)
					return
				}
				desc, err := remote.Get(named, remoteAuthOpt)
				if err != nil {
					log.Printf("reporter: get %s: %v", ref.Source, err)
					return
				}
				seeds, _, isIndex, err := gatherLayers("./store", desc)
				if err != nil {
					log.Printf("reporter: layers %s: %v", ref.Source, err)
					return
				}
				meta[i] = metaResult{desc: desc, seeds: seeds, isIndex: isIndex}
			}(i, ref)
		}
		wg0.Wait()

		// Aggregate all layers (deduplicating shared blobs across images) into one
		// packSink whose onEmit callback POSTs progress frames to the web server.
		now := time.Now()
		sink = &packSink{
			onEmit:   rep.push,
			byDigest: map[string]*layerProg{},
			start:    now,
			prevTime: now,
		}
		for _, m := range meta {
			for _, sd := range m.seeds {
				if _, exists := sink.byDigest[sd.digest]; exists {
					continue // deduplicate layers shared across images
				}
				lp := &layerProg{Index: len(sink.layers), Digest: sd.digest, Size: sd.size, Cached: sd.cached}
				if sd.cached {
					lp.Received = sd.size
					lp.Done = true
					sink.received += sd.size
				}
				sink.total += sd.size
				sink.layers = append(sink.layers, lp)
				sink.byDigest[sd.digest] = lp
			}
		}

		layers := make([]map[string]any, len(sink.layers))
		for i, lp := range sink.layers {
			layers[i] = map[string]any{
				"i": lp.Index, "digest": lp.Digest, "size": lp.Size,
				"received": lp.Received, "cached": lp.Cached, "done": lp.Done,
			}
		}
		watchURL, regErr := rep.register(map[string]any{
			"kind": "pack", "ref": manifestPath, "name": filepath.Base(manifestPath),
			"totalBytes": sink.total, "layers": layers,
		})
		if regErr != nil {
			log.Printf("cli reporter unavailable: %v — continuing without visualization", regErr)
			rep = nil
			sink = nil
		} else {
			log.Printf("watching at %s", watchURL)
			stopTicker = make(chan struct{})
			tickerDone = runTicker(sink.emit, stopTicker)
		}
	}

	// Parallel blob downloads. Each goroutine reuses pre-gathered metadata when
	// the reporter pre-flight ran; otherwise it fetches fresh via remote.Get.
	// Blobs are content-addressed (atomic rename) — safe to write concurrently.
	// index.json is serialized via mu so the store is queryable as images land.
	var mu sync.Mutex
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup

	for i, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, ref ImageRef) {
			defer wg.Done()
			defer func() { <-sem }()

			var desc *remote.Descriptor
			if meta[i].desc != nil {
				desc = meta[i].desc
			} else {
				named, err := name.ParseReference(ref.Source)
				if err != nil {
					log.Printf("failed to parse ref %s: %v", ref.Source, err)
					return
				}
				desc, err = remote.Get(named, remoteAuthOpt)
				if err != nil {
					log.Printf("failed to fetch descriptor %s: %v", ref.Source, err)
					return
				}
			}

			blobPath := filepath.Join("./store", "blobs", desc.Digest.Algorithm, desc.Digest.Hex)
			if _, err := os.Stat(blobPath); err == nil {
				log.Printf("up-to-date, skipping %s (%s)", ref.Rewrite, desc.Digest)
				return
			}

			log.Printf("pulling %s", ref.Source)

			var artifact describable
			switch desc.MediaType {
			case types.OCIImageIndex, types.DockerManifestList:
				idx, err := desc.ImageIndex()
				if err != nil {
					log.Printf("pull failed %s: %v", ref.Source, err)
					return
				}
				var toWrite v1.ImageIndex = idx
				if sink != nil {
					toWrite = progressIndex{idx: idx, sink: sink}
				}
				if err := lyt.WriteIndex(toWrite); err != nil {
					log.Printf("save failed %s: %v", ref.Source, err)
					return
				}
				artifact = idx
			default:
				img, err := pullWithRetry(ref.Source, craneAuthOpt, 7)
				if err != nil {
					log.Printf("pull failed %s: %v", ref.Source, err)
					return
				}
				var toWrite v1.Image = img
				if sink != nil {
					toWrite = progressImage{Image: img, sink: sink}
				}
				if err := lyt.WriteImage(toWrite); err != nil {
					log.Printf("save failed %s: %v", ref.Source, err)
					return
				}
				artifact = img
			}

			d, err := descriptorFor(artifact, ref.Rewrite)
			if err != nil {
				log.Printf("save failed %s: %v", ref.Source, err)
				return
			}

			mu.Lock()
			_ = lyt.RemoveDescriptors(func(existing v1.Descriptor) bool {
				return existing.Annotations[refAnnotationKey] == ref.Rewrite
			})
			if err := lyt.AppendDescriptor(d); err != nil {
				log.Printf("index update failed %s: %v", ref.Rewrite, err)
			}
			mu.Unlock()
			log.Printf("saved %s", ref.Rewrite)
		}(i, ref)
	}

	wg.Wait()

	if stopTicker != nil {
		close(stopTicker)
		<-tickerDone
		sink.emit(true)
		rep.push("done", map[string]any{
			"ok": true, "ref": manifestPath,
			"totalBytes": sink.total,
			"durationMs": time.Since(sink.start).Milliseconds(),
		})
	}

	log.Println("store ready at ./store")
}

func cmdPackCharts(chartsFile string) {
	refs, err := loadChartRefs(chartsFile)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("loaded %d chart refs", len(refs))

	// OCI charts share the same OCI layout as images.
	var lyt layout.Path
	if _, statErr := os.Stat("./store"); os.IsNotExist(statErr) {
		lyt, err = layout.Write("./store", empty.Index)
	} else {
		lyt, err = layout.FromPath("./store")
	}
	if err != nil {
		log.Fatal(err)
	}

	craneAuthOpt := makeAuthOption()
	remoteAuthOpt := makeRemoteAuthOption()
	httpClient := makeHTTPClient()

	var mu sync.Mutex
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup

	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(ref ChartRef) {
			defer wg.Done()
			defer func() { <-sem }()

			switch ref.Type {
			case "oci":
				packOCIChart(ref.OciRef, craneAuthOpt, remoteAuthOpt, lyt, &mu)
			case "http":
				destDir := filepath.Join("./store/helm", ref.RepoName)
				destPath := filepath.Join(destDir, fmt.Sprintf("%s-%s.tgz", ref.Name, ref.Version))
				if _, err := os.Stat(destPath); err == nil {
					log.Printf("up-to-date, skipping %s/%s:%s", ref.RepoName, ref.Name, ref.Version)
					return
				}
				url := strings.TrimSuffix(ref.RepoURL, "/") + "/" + ref.Name + "-" + ref.Version + ".tgz"
				downloadChart(httpClient, url, destDir, destPath, ref.Name, ref.Version)
			case "local":
				destDir := filepath.Join("./store/helm", ref.RepoName)
				destPath := filepath.Join(destDir, filepath.Base(ref.FilePath))
				if _, err := os.Stat(destPath); err == nil {
					log.Printf("up-to-date, skipping %s", filepath.Base(ref.FilePath))
					return
				}
				copyChart(ref.FilePath, destDir, destPath)
			}
		}(ref)
	}

	wg.Wait()
	log.Println("chart store ready")
}

func packOCIChart(ref string, craneAuthOpt crane.Option, remoteAuthOpt remote.Option, lyt layout.Path, mu *sync.Mutex) {
	named, err := name.ParseReference(ref)
	if err != nil {
		log.Printf("failed to parse OCI chart ref %s: %v", ref, err)
		return
	}

	desc, err := remote.Get(named, remoteAuthOpt)
	if err != nil {
		log.Printf("failed to fetch descriptor %s: %v", ref, err)
		return
	}

	blobPath := filepath.Join("./store", "blobs", desc.Digest.Algorithm, desc.Digest.Hex)
	if _, err := os.Stat(blobPath); err == nil {
		log.Printf("up-to-date, skipping OCI chart %s (%s)", ref, desc.Digest)
		return
	}

	img, err := crane.Pull(ref, craneAuthOpt)
	if err != nil {
		log.Printf("pull failed OCI chart %s: %v", ref, err)
		return
	}

	mu.Lock()
	defer mu.Unlock()
	if err := lyt.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": ref,
	})); err != nil {
		log.Printf("save failed OCI chart %s: %v", ref, err)
		return
	}
	log.Printf("saved OCI chart %s", ref)
}

func downloadChart(client *http.Client, url, destDir, destPath, chartName, version string) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		log.Printf("failed to create dir %s: %v", destDir, err)
		return
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Printf("failed to build request for %s: %v", url, err)
		return
	}
	if user, pass := os.Getenv("GAPPY_USER"), os.Getenv("GAPPY_PASS"); user != "" && pass != "" {
		req.SetBasicAuth(user, pass)
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("failed to download %s: %v", url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("failed to download %s: HTTP %d", url, resp.StatusCode)
		return
	}

	f, err := os.Create(destPath)
	if err != nil {
		log.Printf("failed to create %s: %v", destPath, err)
		return
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		log.Printf("failed to write %s: %v", destPath, err)
		os.Remove(destPath)
		return
	}
	log.Printf("downloaded %s:%s", chartName, version)
}

func copyChart(src, destDir, destPath string) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		log.Printf("failed to create dir %s: %v", destDir, err)
		return
	}

	in, err := os.Open(src)
	if err != nil {
		log.Printf("failed to open %s: %v", src, err)
		return
	}
	defer in.Close()

	out, err := os.Create(destPath)
	if err != nil {
		log.Printf("failed to create %s: %v", destPath, err)
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		log.Printf("failed to copy %s: %v", src, err)
		os.Remove(destPath)
		return
	}
	log.Printf("copied %s", filepath.Base(src))
}

func cmdServe(storePath string) {
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

	idxManifest, err := idx.IndexManifest()
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("loading %d manifests into registry...", len(idxManifest.Manifests))

	var wg sync.WaitGroup
	sem := make(chan struct{}, jobs)

	for _, desc := range idxManifest.Manifests {
		wg.Add(1)
		sem <- struct{}{}

		go func(d v1.Descriptor) {
			defer wg.Done()
			defer func() { <-sem }()

			ref := d.Annotations["org.opencontainers.image.ref.name"]
			if ref == "" {
				log.Printf("skipping manifest with no ref annotation: %s", d.Digest)
				return
			}

			dest := fmt.Sprintf("%s/%s", addr, ref)

			switch d.MediaType {
			case types.OCIImageIndex, types.DockerManifestList:
				childIdx, err := idx.ImageIndex(d.Digest)
				if err != nil {
					log.Printf("failed to load index %s: %v", ref, err)
					return
				}
				destRef, err := name.ParseReference(dest, name.Insecure)
				if err != nil {
					log.Printf("failed to parse dest %s: %v", dest, err)
					return
				}
				if err := remote.WriteIndex(destRef, childIdx); err != nil {
					log.Printf("failed to push index %s into registry: %v", ref, err)
					return
				}
			default:
				img, err := idx.Image(d.Digest)
				if err != nil {
					log.Printf("failed to load image %s: %v", ref, err)
					return
				}
				if err := crane.Push(img, dest, crane.Insecure); err != nil {
					log.Printf("failed to push %s into registry: %v", ref, err)
					return
				}
			}

			log.Printf("loaded %s", ref)
		}(desc)
	}

	wg.Wait()

	// Replay finished: start recording genuine client pushes into the store.
	sw.ready.Store(true)
	log.Printf("registry ready on %s — accepting docker push", addr)

	select {}
}

func cmdVersion() {
	fmt.Printf("gappy %s\n", version)
	fmt.Printf("  commit:  %s\n", commit)
	fmt.Printf("  built:   %s\n", date)
	fmt.Printf("  go:      %s\n", runtime.Version())
	fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		log.Fatal("usage:\n  gappy [-j N] pack <images.txt|manifest.yaml>\n  gappy [-j N] pack-charts <found-charts.txt|manifest.yaml>\n  gappy serve [store-path]\n  gappy web [store-path] [listen-addr]\n  gappy discover [dir]\n  gappy split <dvd|dvd9|bd25|bd50|bd100|SIZE> [store] [out]\n  gappy join <out-dir> <disc-001> [disc-002 ...]\n  gappy version")
	}

	switch args[0] {
	case "pack":
		if len(args) < 2 {
			log.Fatal("usage: gappy pack <images.txt|manifest.yaml>")
		}
		cmdPack(args[1], newCLIReporter(webAddr))
	case "pack-charts":
		if len(args) < 2 {
			log.Fatal("usage: gappy pack-charts <found-charts.txt|manifest.yaml>")
		}
		cmdPackCharts(args[1])
	case "serve":
		storePath := "./store"
		if len(args) >= 2 {
			storePath = args[1]
		}
		cmdServe(storePath)
	case "discover":
		root := "."
		if len(args) >= 2 {
			root = args[1]
		}
		cmdDiscover(root)
	case "verify":
		storeDir := "./store"
		if len(args) >= 2 {
			storeDir = args[1]
		}
		cmdVerify(storeDir)
	case "split":
		if len(args) < 2 {
			log.Fatal("usage: gappy split <dvd|dvd9|bd25|bd50|bd100|SIZE> [store-path] [out-dir]")
		}
		storeDir := "./store"
		outDir := "."
		if len(args) >= 3 {
			storeDir = args[2]
		}
		if len(args) >= 4 {
			outDir = args[3]
		}
		cmdSplit(args[1], storeDir, outDir)
	case "join":
		if len(args) < 3 {
			log.Fatal("usage: gappy join <out-dir> <disc-001> [disc-002 ...]")
		}
		cmdJoin(args[1], args[2:])
	case "version":
		cmdVersion()
	case "web":
		fs := flag.NewFlagSet("web", flag.ExitOnError)
		serveFlag := fs.Bool("serve", false, "start the OCI registry immediately on launch")
		_ = fs.Parse(args[1:])
		rest := fs.Args()
		storePath := "./store"
		addr := "127.0.0.1:8080"
		if len(rest) >= 1 {
			storePath = rest[0]
		}
		if len(rest) >= 2 {
			addr = rest[1]
		}
		cmdWeb(storePath, addr, *serveFlag)
	default:
		log.Fatalf("unknown command %q — use pack, pack-charts, serve, web, discover, or version", args[0])
	}
}
