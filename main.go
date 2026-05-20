package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"gopkg.in/yaml.v3"
)

var jobs = runtime.NumCPU()

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
			img, err := crane.Pull(ref.Source, authOpt)
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

	reg := registry.New(registry.WithReferrersSupport(false))
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

	for _, desc := range idxManifest.Manifests {
		ref := desc.Annotations["org.opencontainers.image.ref.name"]
		if ref == "" {
			log.Printf("skipping manifest with no ref annotation: %s", desc.Digest)
			continue
		}

		img, err := idx.Image(desc.Digest)
		if err != nil {
			log.Printf("failed to load image %s: %v", ref, err)
			continue
		}

		dest := fmt.Sprintf("%s/%s", addr, ref)
		if err := crane.Push(img, dest, crane.Insecure); err != nil {
			log.Printf("failed to push %s into registry: %v", ref, err)
			continue
		}
		log.Printf("loaded %s", ref)
	}

	log.Printf("registry ready on %s", addr)
	select {}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage:\n  gappy pack <images.txt|manifest.yaml>\n  gappy serve [store-path]")
	}

	switch os.Args[1] {
	case "pack":
		if len(os.Args) < 3 {
			log.Fatal("usage: gappy pack <images.txt|manifest.yaml>")
		}
		cmdPack(os.Args[2])

	case "serve":
		storePath := "./store"
		if len(os.Args) >= 3 {
			storePath = os.Args[2]
		}
		cmdServe(storePath)

	default:
		log.Fatalf("unknown command %q — use pack or serve", os.Args[1])
	}
}
