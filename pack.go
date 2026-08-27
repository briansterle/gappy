package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"gopkg.in/yaml.v3"
)

// --- manifests and refs ----------------------------------------------------

// HaulerImage and HaulerChart are named rather than inlined into the manifest
// structs so a filtered copy of Spec.Images or Spec.Charts can be built without
// restating the field tags — gappy diff rewrites both.
type HaulerImage struct {
	Name    string `yaml:"name"`
	Rewrite string `yaml:"rewrite"`
}

type HaulerChart struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	RepoURL string `yaml:"repoURL"`
}

type HaulerManifest struct {
	Spec struct {
		Images []HaulerImage `yaml:"images"`
	} `yaml:"spec"`
}

type HaulerChartManifest struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Charts []HaulerChart `yaml:"charts"`
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

type HelmRepositoriesFile struct {
	Repositories []HelmRepository `yaml:"repositories"`
}

type HelmRepository struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

func loadHelmRepos() []HelmRepository {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".config", "helm", "repositories.yaml"))
	if err != nil {
		return nil
	}
	var f HelmRepositoriesFile
	yaml.Unmarshal(data, &f)
	return f.Repositories
}

func resolveHelmAlias(alias string, repos []HelmRepository) *HelmRepository {
	name := strings.TrimPrefix(alias, "@")
	for i := range repos {
		if repos[i].Name == name {
			return &repos[i]
		}
	}
	return nil
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

// --- discover --------------------------------------------------------------

var imageRef = regexp.MustCompile(
	`(?i)image\s*[:=]\s*["']?([a-zA-Z0-9][a-zA-Z0-9._/:-]+)["']?`,
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
}

var scanExts = map[string]bool{
	".yaml": true, ".yml": true, ".json": true,
	".toml": true, ".hcl": true, ".tf": true,
	".env": true, ".txt": true,
	"": true,
}

type ChartFile struct {
	Dependencies []struct {
		Name       string `yaml:"name"`
		Repository string `yaml:"repository"`
		Version    string `yaml:"version"`
	} `yaml:"dependencies"`
}

func cmdDiscover(root string) {
	if root == "" {
		root = "."
	}

	helmRepos := loadHelmRepos()
	seen := map[string]bool{}
	seenCharts := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		if d.Name() == "Chart.yaml" {
			charts, err := findChartsInFile(path, helmRepos)
			if err != nil {
				log.Printf("skipping %s: %v", path, err)
			} else {
				for _, ref := range charts {
					seenCharts[ref] = true
				}
			}
		}

		// Collect pre-downloaded .tgz chart files sitting in any charts/ subdirectory.
		// These are the result of a prior `helm dep update` and are ready to copy directly.
		if filepath.Ext(d.Name()) == ".tgz" && filepath.Base(filepath.Dir(path)) == "charts" {
			repoName := tgzRepoName(path, helmRepos)
			seenCharts[fmt.Sprintf("local::%s::%s", repoName, path)] = true
		}

		if !scanExts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}

		refs, err := findRefsInFile(path)
		if err != nil {
			log.Printf("skipping %s: %v", path, err)
			return nil
		}
		for _, ref := range refs {
			seen[ref] = true
		}
		return nil
	})
	if err != nil {
		log.Printf("walk error: %v", err)
	}

	writeFound("found-images.txt", seen, "image refs", "no image references found")
	writeFound("found-charts.txt", seenCharts, "chart refs", "no chart references found")
}

func writeFound(path string, set map[string]bool, label, emptyMsg string) {
	items := make([]string, 0, len(set))
	for item := range set {
		items = append(items, item)
	}
	sort.Strings(items)

	if len(items) == 0 {
		log.Println(emptyMsg)
		return
	}

	out, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	for _, item := range items {
		fmt.Fprintln(w, item)
	}
	if err := w.Flush(); err != nil {
		log.Fatal(err)
	}
	log.Printf("discovered %d unique %s → %s", len(items), label, path)
}

func findRefsInFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var refs []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		m := imageRef.FindStringSubmatch(scanner.Text())
		if m != nil {
			refs = append(refs, m[1])
		}
	}
	return refs, scanner.Err()
}

// findChartsInFile parses a Chart.yaml and emits tagged lines for found-charts.txt:
//
//	oci::{full-oci-ref}
//	http::{repoName}::{repoURL}::{chartName}::{version}
//
// It resolves @alias repos via ~/.config/helm/repositories.yaml and handles
// bare oci:// and https:// URLs directly. file:// local deps are skipped.
func findChartsInFile(path string, repos []HelmRepository) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var chart ChartFile
	if err := yaml.Unmarshal(data, &chart); err != nil {
		return nil, err
	}

	var refs []string
	for _, dep := range chart.Dependencies {
		repo := dep.Repository

		switch {
		case strings.HasPrefix(repo, "oci://"):
			base := strings.TrimPrefix(repo, "oci://")
			refs = append(refs, fmt.Sprintf("oci::%s/%s:%s", base, dep.Name, dep.Version))

		case strings.HasPrefix(repo, "@"):
			r := resolveHelmAlias(repo, repos)
			if r == nil {
				log.Printf("skipping %s — alias %s not in ~/.config/helm/repositories.yaml", dep.Name, repo)
				continue
			}
			if strings.HasPrefix(r.URL, "oci://") {
				base := strings.TrimPrefix(r.URL, "oci://")
				refs = append(refs, fmt.Sprintf("oci::%s/%s:%s", base, dep.Name, dep.Version))
			} else {
				refs = append(refs, fmt.Sprintf("http::%s::%s::%s::%s", r.Name, r.URL, dep.Name, dep.Version))
			}

		case strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "http://"):
			repoName := filepath.Base(strings.TrimSuffix(repo, "/"))
			refs = append(refs, fmt.Sprintf("http::%s::%s::%s::%s", repoName, repo, dep.Name, dep.Version))

		case strings.HasPrefix(repo, "file://"):
			// local sub-chart reference — not a remote artifact to pack
		}
	}
	return refs, nil
}

// tgzRepoName traces a pre-downloaded .tgz back to its Helm alias by reading
// the parent Chart.yaml and matching the filename to a dependency entry.
func tgzRepoName(tgzPath string, repos []HelmRepository) string {
	// e.g. charts/myapp/charts/my-chart-1.2.3.tgz → charts/myapp/Chart.yaml
	chartYaml := filepath.Join(filepath.Dir(filepath.Dir(tgzPath)), "Chart.yaml")
	data, err := os.ReadFile(chartYaml)
	if err != nil {
		return "local"
	}
	var chart ChartFile
	yaml.Unmarshal(data, &chart)

	base := filepath.Base(tgzPath)
	for _, dep := range chart.Dependencies {
		if base == fmt.Sprintf("%s-%s.tgz", dep.Name, dep.Version) {
			if strings.HasPrefix(dep.Repository, "@") {
				if r := resolveHelmAlias(dep.Repository, repos); r != nil {
					return r.Name
				}
			}
		}
	}
	return "local"
}

// --- registry credentials --------------------------------------------------

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

// --- pack images -----------------------------------------------------------

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
		Annotations: map[string]string{refAnnotationKey: ref},
	}, nil
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

func cmdPack(manifestPath string) {
	useStore("./store")
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

	// Parallel blob downloads. Blobs are content-addressed (atomic rename) —
	// safe to write concurrently. index.json is serialized via mu so the store
	// is queryable as images land.
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

			var artifact describable
			switch desc.MediaType {
			case types.OCIImageIndex, types.DockerManifestList:
				idx, err := desc.ImageIndex()
				if err != nil {
					log.Printf("pull failed %s: %v", ref.Source, err)
					return
				}
				if err := lyt.WriteIndex(idx); err != nil {
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
				if err := lyt.WriteImage(img); err != nil {
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
		}(ref)
	}

	wg.Wait()

	normalizeStoreFiles("./store")
	log.Println("store ready at ./store")
}

// --- pack charts -----------------------------------------------------------

func cmdPackCharts(chartsFile string) {
	useStore("./store")
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
	normalizeStoreFiles("./store")
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
		refAnnotationKey: ref,
	})); err != nil {
		log.Printf("save failed OCI chart %s: %v", ref, err)
		return
	}
	log.Printf("saved OCI chart %s", ref)
}

func downloadChart(client *http.Client, url, destDir, destPath, chartName, version string) {
	if err := perm.mkdirAll(destDir); err != nil {
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
	if err := perm.chmodFile(destPath); err != nil {
		log.Printf("warning: chmod %s: %v", destPath, err)
	}
	log.Printf("downloaded %s:%s", chartName, version)
}

func copyChart(src, destDir, destPath string) {
	if err := perm.mkdirAll(destDir); err != nil {
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
	if err := perm.chmodFile(destPath); err != nil {
		log.Printf("warning: chmod %s: %v", destPath, err)
	}
	log.Printf("copied %s", filepath.Base(src))
}
