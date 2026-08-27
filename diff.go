package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// --- baseline keys ---------------------------------------------------------

// A baseline is reduced to a set of string keys, and an entry in the current
// manifest is dropped when any of the keys it could be known by is present.
// One item yields several keys because the same chart is written a different
// way in each place it appears — my-chart-1.0.0.tgz in a store, my-chart:1.0.0
// in an OCI ref, http::repo::url::my-chart::1.0.0 in found-charts.txt — and a
// key set makes those interchangeable without parsing every form into a
// canonical one.
//
// Recall matters asymmetrically here. A key the baseline should have had but
// doesn't leaves an item in the delta that was already shipped: wasteful, but
// the bundle is still correct. A key that collides with an item the baseline
// does not actually contain drops that item silently, and the gap only turns
// up in the airgap. So this file over-collects rather than under-collects, and
// refuses to guess where a guess could produce the second kind of error.

// manifestFileNames are the fixed names gappy itself writes.
var manifestFileNames = map[string]bool{
	"found-images.txt": true,
	"images.txt":       true,
	"found-charts.txt": true,
}

// isPlainManifestName reports whether base is a file whose every line is meant
// to be read as a ref. Only these get the line-per-key treatment; an arbitrary
// text file would inject its prose as keys.
func isPlainManifestName(base string) bool { return manifestFileNames[base] }

// isYAMLName reports whether base should be tried as a Hauler manifest. Any
// .yaml is worth parsing, because a Hauler manifest can be named anything, but
// one that doesn't parse as Hauler contributes nothing — see collectFromContent.
func isYAMLName(base string) bool {
	return strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml")
}

// collectBaselineKeys extracts the reference keys already covered by a
// baseline, which may be a .zip archive, an OCI store or directory tree, or a
// single manifest file.
func collectBaselineKeys(baselinePath string) (map[string]bool, error) {
	fi, err := os.Stat(baselinePath)
	if err != nil {
		return nil, fmt.Errorf("stat baseline %s: %w", baselinePath, err)
	}

	switch {
	case fi.IsDir():
		return collectKeysFromDir(baselinePath)
	case strings.HasSuffix(baselinePath, ".zip"):
		return collectKeysFromZip(baselinePath)
	default:
		return collectKeysFromFile(baselinePath)
	}
}

func collectKeysFromZip(zipPath string) (map[string]bool, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer r.Close()

	keys := make(map[string]bool)
	for _, f := range r.File {
		base := filepath.Base(f.Name)

		switch {
		case base == "index.json":
			data, err := readZipEntry(f)
			if err != nil {
				log.Printf("warning: baseline %s!%s: %v", zipPath, f.Name, err)
				continue
			}
			var idx ociIndexJSON
			if err := json.Unmarshal(data, &idx); err != nil {
				// Not every index.json in a bundle is an OCI index.
				continue
			}
			collectFromOCIIndex(idx, keys)

		case isHelmArchive(f.Name):
			collectFromChartArchive(f.Name, keys)

		case isPlainManifestName(base) || isYAMLName(base):
			data, err := readZipEntry(f)
			if err != nil {
				log.Printf("warning: baseline %s!%s: %v", zipPath, f.Name, err)
				continue
			}
			collectFromContent(data, keys, isPlainManifestName(base))
		}
	}
	return keys, nil
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func collectKeysFromDir(dirPath string) (map[string]bool, error) {
	keys := make(map[string]bool)

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory gappy can't descend into is a hole in the baseline,
			// which costs recall, not correctness — report it and continue.
			log.Printf("warning: baseline %s: %v", path, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()

		switch {
		case base == "index.json":
			idx, err := readStoreIndex(filepath.Dir(path))
			if err != nil {
				log.Printf("warning: baseline %s: %v", path, err)
				return nil
			}
			collectFromOCIIndex(idx, keys)

		case isHelmArchive(path):
			collectFromChartArchive(path, keys)

		case isPlainManifestName(base) || isYAMLName(base):
			data, err := os.ReadFile(path)
			if err != nil {
				log.Printf("warning: baseline %s: %v", path, err)
				return nil
			}
			collectFromContent(data, keys, isPlainManifestName(base))
		}
		return nil
	})

	return keys, err
}

func collectKeysFromFile(filePath string) (map[string]bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read baseline %s: %w", filePath, err)
	}
	keys := make(map[string]bool)
	// Named explicitly by the user, so its lines are refs whatever it's called.
	collectFromContent(data, keys, !isYAMLName(filepath.Base(filePath)))
	return keys, nil
}

// isHelmArchive reports whether path is a packaged chart sitting in a helm
// directory. The directory check keeps an unrelated .tgz from being read as a
// chart named after its file.
func isHelmArchive(path string) bool {
	return strings.HasSuffix(path, ".tgz") && strings.Contains(filepath.ToSlash(path), "helm/")
}

func collectFromOCIIndex(idx ociIndexJSON, keys map[string]bool) {
	for _, d := range idx.Manifests {
		if ref := d.Annotations[refAnnotationKey]; ref != "" {
			keys[ref] = true
		}
	}
}

// collectFromChartArchive keys a packaged chart by its file name, by the
// helm/-relative path it was stored under, and by the bare name-version.
func collectFromChartArchive(path string, keys map[string]bool) {
	slashed := filepath.ToSlash(path)
	base := filepath.Base(slashed)
	keys[base] = true
	keys[strings.TrimSuffix(base, ".tgz")] = true
	if _, rel, ok := strings.Cut(slashed, "helm/"); ok {
		keys["helm/"+rel] = true
	}
}

// collectFromContent parses data as a Hauler image or chart manifest and keys
// every entry.
//
// allowPlainText enables the one-key-per-line fallback, and is false for files
// found by extension rather than by name. An arbitrary YAML that isn't a Hauler
// manifest must not have its raw lines injected as keys: a stray `nginx:1.21`
// in some values.yaml would then mask a real image and silently drop it from
// the delta, which is the one failure mode this command cannot have.
func collectFromContent(data []byte, keys map[string]bool, allowPlainText bool) {
	var imgManifest HaulerManifest
	if err := yaml.Unmarshal(data, &imgManifest); err == nil && len(imgManifest.Spec.Images) > 0 {
		for _, img := range imgManifest.Spec.Images {
			if img.Name != "" {
				keys[img.Name] = true
			}
			if img.Rewrite != "" {
				keys[img.Rewrite] = true
			}
		}
		return
	}

	var chartManifest HaulerChartManifest
	if err := yaml.Unmarshal(data, &chartManifest); err == nil && chartManifest.Kind == "Charts" && len(chartManifest.Spec.Charts) > 0 {
		for _, c := range chartManifest.Spec.Charts {
			for _, k := range chartKeys(c.RepoURL, c.Name, c.Version) {
				keys[k] = true
			}
		}
		return
	}

	if !allowPlainText {
		return
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), maxManifestLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys[line] = true
		for _, k := range chartRefKeys(line) {
			keys[k] = true
		}
	}
	if err := scanner.Err(); err != nil {
		// Truncating here would silently shrink the baseline and pad the delta
		// with things already shipped, so say so rather than return short.
		log.Printf("warning: baseline manifest truncated: %v", err)
	}
}

// --- ref keys --------------------------------------------------------------

// maxManifestLine caps a manifest line. bufio.Scanner's 64KB default stops the
// scan silently, and a long line is likelier a wrong file than a real ref.
const maxManifestLine = 1 << 20

// chartKeys renders the interchangeable spellings of one chart.
func chartKeys(repoURL, name, version string) []string {
	return []string{
		fmt.Sprintf("%s-%s.tgz", name, version),
		fmt.Sprintf("%s:%s", name, version),
		fmt.Sprintf("%s/%s:%s", repoURL, name, version),
	}
}

// chartRefKeys renders the alternate spellings of one found-charts.txt line.
// The line itself is always a key; these are the additional forms the same
// chart appears as elsewhere. A line that isn't a typed chart ref yields none —
// an image ref is only ever known by itself.
func chartRefKeys(line string) []string {
	parts := strings.Split(line, "::")
	switch {
	case strings.HasPrefix(line, "oci::") && len(parts) >= 2:
		return []string{parts[1]}
	case strings.HasPrefix(line, "http::") && len(parts) >= 5:
		return chartKeys(parts[1], parts[3], parts[4])
	case strings.HasPrefix(line, "local::") && len(parts) >= 3:
		return []string{filepath.Base(parts[2])}
	}
	return nil
}

// coveredBy reports whether the baseline already holds line, under its own
// spelling or any equivalent one.
func coveredBy(keys map[string]bool, line string) bool {
	if keys[line] {
		return true
	}
	for _, k := range chartRefKeys(line) {
		if keys[k] {
			return true
		}
	}
	return false
}

// --- diff ------------------------------------------------------------------

// cmdDiff filters currentPath against baselinePath and writes the entries the
// baseline doesn't already cover to outPath, or to stdout when outPath is "".
func cmdDiff(baselinePath, currentPath, outPath string) {
	baselineKeys, err := collectBaselineKeys(baselinePath)
	if err != nil {
		log.Fatalf("failed to read baseline %s: %v", baselinePath, err)
	}
	if len(baselineKeys) == 0 {
		// Not fatal — an empty baseline is legitimate for a first bundle — but
		// it's indistinguishable in the output from a baseline that was read
		// wrong, and the whole current manifest is about to come back as delta.
		log.Printf("warning: no known refs found in baseline %s — every entry will be reported as missing", baselinePath)
	}

	currentData, err := os.ReadFile(currentPath)
	if err != nil {
		log.Fatalf("failed to read current manifest %s: %v", currentPath, err)
	}

	if isYAMLName(currentPath) {
		var imgManifest HaulerManifest
		if err := yaml.Unmarshal(currentData, &imgManifest); err == nil && len(imgManifest.Spec.Images) > 0 {
			diffHaulerImages(imgManifest, baselineKeys, currentPath, outPath)
			return
		}

		var chartManifest HaulerChartManifest
		if err := yaml.Unmarshal(currentData, &chartManifest); err == nil && chartManifest.Kind == "Charts" && len(chartManifest.Spec.Charts) > 0 {
			diffHaulerCharts(chartManifest, baselineKeys, currentPath, outPath)
			return
		}

		log.Fatalf("%s is not a Hauler image or chart manifest", currentPath)
	}

	diffTextManifest(currentData, baselineKeys, currentPath, outPath)
}

func diffHaulerImages(manifest HaulerManifest, baselineKeys map[string]bool, currentPath, outPath string) {
	total := len(manifest.Spec.Images)

	remaining := make([]HaulerImage, 0, total)
	for _, img := range manifest.Spec.Images {
		if !coveredBy(baselineKeys, img.Name) {
			remaining = append(remaining, img)
		}
	}

	manifest.Spec.Images = remaining
	outData, err := yaml.Marshal(manifest)
	if err != nil {
		log.Fatalf("failed to marshal diff YAML: %v", err)
	}
	writeDiffOutput(outData, currentPath, outPath, total, len(remaining))
}

func diffHaulerCharts(manifest HaulerChartManifest, baselineKeys map[string]bool, currentPath, outPath string) {
	total := len(manifest.Spec.Charts)

	remaining := make([]HaulerChart, 0, total)
	for _, c := range manifest.Spec.Charts {
		covered := false
		for _, k := range chartKeys(c.RepoURL, c.Name, c.Version) {
			if baselineKeys[k] {
				covered = true
				break
			}
		}
		if !covered {
			remaining = append(remaining, c)
		}
	}

	manifest.Spec.Charts = remaining
	outData, err := yaml.Marshal(manifest)
	if err != nil {
		log.Fatalf("failed to marshal diff YAML: %v", err)
	}
	writeDiffOutput(outData, currentPath, outPath, total, len(remaining))
}

// diffTextManifest filters a found-images.txt or found-charts.txt, keeping
// blank lines and comments so the delta stays as readable as its source.
func diffTextManifest(data []byte, baselineKeys map[string]bool, currentPath, outPath string) {
	var out bytes.Buffer
	total, kept := 0, 0

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), maxManifestLine)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			total++
			if coveredBy(baselineKeys, trimmed) {
				continue
			}
			kept++
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("failed to read %s: %v", currentPath, err)
	}

	writeDiffOutput(out.Bytes(), currentPath, outPath, total, kept)
}

// writeDiffOutput reports the tally and writes data, renaming into place so a
// failed run can't leave a half-written manifest that a later pack would read.
func writeDiffOutput(data []byte, currentPath, outPath string, total, deltaCount int) {
	log.Printf("diff %s: %d total items -> %d missing from baseline", filepath.Base(currentPath), total, deltaCount)

	if outPath == "" {
		os.Stdout.Write(data)
		return
	}

	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, data, perm.file); err != nil {
		log.Fatalf("failed to write output %s: %v", tmp, err)
	}
	// os.WriteFile is subject to the umask; a manifest the next person can't
	// read is the same trap fix-perms exists for.
	if err := perm.chmodFile(tmp); err != nil {
		log.Printf("warning: chmod %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		log.Fatalf("failed to rename output to %s: %v", outPath, err)
	}
}
