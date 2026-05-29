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

type HelmRepositoriesFile struct {
	Repositories []HelmRepository `yaml:"repositories"`
}

type HelmRepository struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
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
