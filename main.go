package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"gopkg.in/yaml.v3"
)

var jobs int

// Set at build time via -ldflags.
var (
	version = "dev"
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

func (h *layoutBlobHandler) Get(ctx context.Context, repo string, hash v1.Hash) (io.ReadCloser, error) {
	path := filepath.Join(h.blobsDir, hash.Algorithm, hash.Hex)
	return os.Open(path)
}

func (h *layoutBlobHandler) Stat(ctx context.Context, repo string, hash v1.Hash) (int64, error) {
	path := filepath.Join(h.blobsDir, hash.Algorithm, hash.Hex)
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (h *layoutBlobHandler) Put(ctx context.Context, repo string, hash v1.Hash, rc io.ReadCloser) error {
	defer rc.Close()
	return nil
}

// helmRepoHandler serves a Helm HTTP repository from a local directory.
// It generates index.yaml on the fly from the .tgz files present.
type helmRepoHandler struct {
	dir string
}

func (h *helmRepoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "index.yaml":
		data, err := buildHelmIndex(h.dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-yaml")
		w.Write(data)

	case strings.HasSuffix(p, ".tgz"):
		f, err := os.Open(filepath.Join(h.dir, filepath.Base(p)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, f)

	default:
		http.NotFound(w, r)
	}
}

func init() {
	flag.IntVar(&jobs, "j", max(1, runtime.NumCPU()-1), "parallel jobs")
	flag.Parse()
}

func makeAuthOption() crane.Option {
	user := os.Getenv("RSART_LOCAL_USER")
	pass := os.Getenv("RSART_LOCAL_AUTH")
	if user != "" && pass != "" {
		return crane.WithAuth(authn.FromConfig(authn.AuthConfig{
			Username: user,
			Password: pass,
		}))
	}
	return crane.WithAuthFromKeychain(authn.DefaultKeychain)
}

func makeRemoteAuthOption() remote.Option {
	user := os.Getenv("RSART_LOCAL_USER")
	pass := os.Getenv("RSART_LOCAL_AUTH")
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
	repos := loadHelmRepos()
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

func cmdPack(manifestPath string) {
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
	var mu sync.Mutex
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup

	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(ref ImageRef) {
			defer wg.Done()
			defer func() { <-sem }()

			named, err := name.ParseReference(ref.Source)
			if err != nil {
				log.Printf("failed to parse ref %s: %v", ref.Source, err)
				return
			}

			desc, err := remote.Get(named, remoteAuthOpt)
			if err != nil {
				log.Printf("failed to fetch descriptor %s: %v", ref.Source, err)
				return
			}

			blobPath := filepath.Join("./store", "blobs", desc.Digest.Algorithm, desc.Digest.Hex)
			if _, err := os.Stat(blobPath); err == nil {
				log.Printf("up-to-date, skipping %s (%s)", ref.Rewrite, desc.Digest)
				return
			}

			log.Printf("pulling %s", ref.Source)

			switch desc.MediaType {
			case types.OCIImageIndex, types.DockerManifestList:
				idx, err := desc.ImageIndex()
				if err != nil {
					log.Printf("pull failed %s: %v", ref.Source, err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if err := lyt.AppendIndex(idx, layout.WithAnnotations(map[string]string{
					"org.opencontainers.image.ref.name": ref.Rewrite,
				})); err != nil {
					log.Printf("save failed %s: %v", ref.Source, err)
					return
				}
			default:
				img, err := pullWithRetry(ref.Source, craneAuthOpt, 7)
				if err != nil {
					log.Printf("pull failed %s: %v", ref.Source, err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if err := lyt.AppendImage(img, layout.WithAnnotations(map[string]string{
					"org.opencontainers.image.ref.name": ref.Rewrite,
				})); err != nil {
					log.Printf("save failed %s: %v", ref.Source, err)
					return
				}
			}
			log.Printf("saved %s", ref.Rewrite)
		}(ref)
	}

	wg.Wait()
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
	if user, pass := os.Getenv("RSART_LOCAL_USER"), os.Getenv("RSART_LOCAL_AUTH"); user != "" && pass != "" {
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
	idx, err := layout.ImageIndexFromPath(storePath)
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

	mux := http.NewServeMux()
	mux.Handle("/v2/", reg)

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
	log.Printf("registry ready on %s", addr)

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
	args := flag.Args()
	if len(args) < 1 {
		log.Fatal("usage:\n  gappy [-j N] pack <images.txt|manifest.yaml>\n  gappy [-j N] pack-charts <found-charts.txt|manifest.yaml>\n  gappy serve [store-path]\n  gappy discover [dir]\n  gappy version")
	}

	switch args[0] {
	case "pack":
		if len(args) < 2 {
			log.Fatal("usage: gappy pack <images.txt|manifest.yaml>")
		}
		cmdPack(args[1])
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
	case "version":
		cmdVersion()
	default:
		log.Fatalf("unknown command %q — use pack, pack-charts, serve, discover, or version", args[0])
	}
}
