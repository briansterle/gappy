package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"io"
	"os"
	"runtime"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"gopkg.in/yaml.v3"
)

var jobs int

type HaulerManifest struct {
	Spec struct {
		Images []struct {
			Name    string `yaml:"name"`
			Rewrite string `yaml:"rewrite"`
		} `yaml:"images"`
	} `yaml:"spec"`
}

type ImageRef struct {
	Source  string
	Rewrite string
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

// Put satisfies the registry interface, but we won't actually need it.
func (h *layoutBlobHandler) Put(ctx context.Context, repo string, hash v1.Hash, rc io.ReadCloser) error {
	defer rc.Close()
	return nil
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
	refs, err := loadRefs(manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("loaded %d image refs", len(refs))

	os.RemoveAll("./store")
	lyt, err := layout.Write("./store", empty.Index)
	if err != nil {
		log.Fatal(err)
	}

	authOpt := makeAuthOption()
	var mu sync.Mutex
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup

	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func(ref ImageRef) {
			defer wg.Done()
			defer func() { <-sem }()

			log.Printf("pulling %s", ref.Source)
			img, err := pullWithRetry(ref.Source, authOpt, 7)
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
			} else {
				log.Printf("saved %s", ref.Rewrite)
			}
		}(ref)
	}

	wg.Wait()
	log.Println("store ready at ./store")
}

func cmdServe(storePath string) {
	idx, err := layout.ImageIndexFromPath(storePath)
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}

	// Point the registry's storage directly at the OCI layout's blob folder
	// The registry will recognize all existing blobs instantly.
	blobsDir := fmt.Sprintf("%s/blobs", storePath)
	log.Printf("serving blobs explicitly from %s", blobsDir)

	// Use our explicit OCI layout handler
	handler := &layoutBlobHandler{blobsDir: blobsDir}

	reg := registry.New(
		registry.WithReferrersSupport(false),
		registry.WithBlobHandler(handler),
	)
	addr := "127.0.0.1:5000"

	server := &http.Server{Addr: addr, Handler: reg}
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

			img, err := idx.Image(d.Digest)
			if err != nil {
				log.Printf("failed to load image %s: %v", ref, err)
				return
			}

			dest := fmt.Sprintf("%s/%s", addr, ref)
			
			// Because the blobs already exist in the handler, crane.Push will detect 
			// them and ONLY push the lightweight image manifests.
			if err := crane.Push(img, dest, crane.Insecure); err != nil {
				log.Printf("failed to push %s into registry: %v", ref, err)
				return
			}
			log.Printf("loaded %s", ref)
		}(desc)
	}

	wg.Wait()
	log.Printf("registry ready on %s", addr)
	
	select {}
}

func main() {
	args := flag.Args()
	if len(args) < 1 {
		log.Fatal("usage:\n  gappy [-j N] pack <images.txt|manifest.yaml>\n  gappy serve [store-path]")
	}

	switch args[0] {
	case "pack":
		if len(args) < 2 {
			log.Fatal("usage: gappy pack <images.txt|manifest.yaml>")
		}
		cmdPack(args[1])
	case "serve":
		storePath := "./store"
		if len(args) >= 2 {
			storePath = args[1]
		}
		cmdServe(storePath)
	default:
		log.Fatalf("unknown command %q — use pack or serve", args[0])
	}
}
