package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func helmRegistry() string {
	r := os.Getenv("RSART_HELM_REGISTRY")
	return strings.TrimPrefix(strings.TrimSuffix(r, "/"), "oci://")
}

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
			charts, err := findChartsInFile(path)
			if err != nil {
				log.Printf("skipping %s: %v", path, err)
			} else {
				for _, ref := range charts {
					seenCharts[ref] = true
				}
			}
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

	// write found-images.txt
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	if len(refs) == 0 {
		log.Println("no image references found")
	} else {
		out, err := os.Create("found-images.txt")
		if err != nil {
			log.Fatal(err)
		}
		defer out.Close()
		w := bufio.NewWriter(out)
		for _, ref := range refs {
			fmt.Fprintln(w, ref)
		}
		if err := w.Flush(); err != nil {
			log.Fatal(err)
		}
		log.Printf("discovered %d unique image refs → found-images.txt", len(refs))
	}

	// write found-charts.txt
	chartRefs := make([]string, 0, len(seenCharts))
	for ref := range seenCharts {
		chartRefs = append(chartRefs, ref)
	}
	sort.Strings(chartRefs)

	if len(chartRefs) == 0 {
		log.Println("no OCI chart references found")
	} else {
		out, err := os.Create("found-charts.txt")
		if err != nil {
			log.Fatal(err)
		}
		defer out.Close()
		w := bufio.NewWriter(out)
		for _, ref := range chartRefs {
			fmt.Fprintln(w, ref)
		}
		if err := w.Flush(); err != nil {
			log.Fatal(err)
		}
		log.Printf("discovered %d unique chart refs → found-charts.txt", len(chartRefs))
	}
}

func findChartsInFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var chart ChartFile
	if err := yaml.Unmarshal(data, &chart); err != nil {
		return nil, err
	}

	registry := helmRegistry()

	var refs []string
	for _, dep := range chart.Dependencies {
		repo := dep.Repository

		if strings.HasPrefix(repo, "@") {
			if registry == "" {
				log.Printf("skipping %s — RSART_HELM_REGISTRY not set", dep.Name)
				continue
			}
			repo = registry
		} else if strings.HasPrefix(repo, "oci://") {
			repo = strings.TrimPrefix(repo, "oci://")
		} else {
			continue
		}

		refs = append(refs, fmt.Sprintf("%s/%s:%s", repo, dep.Name, dep.Version))
	}
	return refs, nil
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
