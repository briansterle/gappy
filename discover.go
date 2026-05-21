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
)

// imageRef matches lines like:
//   image: registry/foo:latest
//   image: "registry/foo:latest"
//   image = registry/foo:latest   (Nomad HCL)
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
	"": true, // extensionless: Dockerfiles, Makefiles, etc.
}

func cmdDiscover(root string) {
	if root == "" {
		root = "."
	}

	seen := map[string]bool{}

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

	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	if len(refs) == 0 {
		log.Println("no image references found")
		return
	}

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
