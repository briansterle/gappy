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

// normalizeImageKey trims registry prefixes and tags where appropriate
func normalizeImageKey(ref string) string {
	ref = strings.TrimSpace(ref)
	return ref
}

// collectBaselineKeys extracts known image and chart reference keys from a baseline
// which can be a .zip archive, a directory, or a manifest file.
func collectBaselineKeys(baselinePath string) (map[string]bool, error) {
	fi, err := os.Stat(baselinePath)
	if err != nil {
		return nil, fmt.Errorf("stat baseline %s: %w", baselinePath, err)
	}

	if !fi.IsDir() && (strings.HasSuffix(baselinePath, ".zip") || strings.HasSuffix(baselinePath, ".tar.gz")) {
		if strings.HasSuffix(baselinePath, ".zip") {
			return collectKeysFromZip(baselinePath)
		}
	}

	if fi.IsDir() {
		return collectKeysFromDir(baselinePath)
	}

	return collectKeysFromFile(baselinePath)
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

		// 1. Check index.json descriptors for image and OCI chart refs
		if base == "index.json" {
			rc, err := f.Open()
			if err == nil {
				var idx ociIndexJSON
				if err := json.NewDecoder(rc).Decode(&idx); err == nil {
					for _, d := range idx.Manifests {
						if ref := d.Annotations[refAnnotationKey]; ref != "" {
							keys[ref] = true
							keys[normalizeImageKey(ref)] = true
						}
					}
				}
				rc.Close()
			}
		}

		// 2. Check helm/*.tgz files
		if strings.Contains(f.Name, "helm/") && strings.HasSuffix(f.Name, ".tgz") {
			keys[base] = true
			parts := strings.Split(f.Name, "helm/")
			if len(parts) > 1 {
				keys["helm/"+parts[1]] = true
			}
			// name-version.tgz
			nameVer := strings.TrimSuffix(base, ".tgz")
			keys[nameVer] = true
		}

		// 3. Parse manifest text/yaml files embedded in the zip
		if base == "found-images.txt" || base == "images.txt" || base == "found-charts.txt" ||
			strings.HasSuffix(base, "-manifest.yaml") || strings.HasSuffix(base, "-manifest.yml") {
			rc, err := f.Open()
			if err == nil {
				data, err := io.ReadAll(rc)
				rc.Close()
				if err == nil {
					extractKeysFromContent(base, data, keys)
				}
			}
		}
	}

	return keys, nil
}

func collectKeysFromDir(dirPath string) (map[string]bool, error) {
	keys := make(map[string]bool)

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := d.Name()

		if base == "index.json" {
			idx, err := readStoreIndex(filepath.Dir(path))
			if err == nil {
				for _, m := range idx.Manifests {
					if ref := m.Annotations[refAnnotationKey]; ref != "" {
						keys[ref] = true
						keys[normalizeImageKey(ref)] = true
					}
				}
			}
		}

		if strings.HasSuffix(base, ".tgz") && strings.Contains(path, "helm") {
			keys[base] = true
			nameVer := strings.TrimSuffix(base, ".tgz")
			keys[nameVer] = true
		}

		if base == "found-images.txt" || base == "images.txt" || base == "found-charts.txt" ||
			strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") {
			data, err := os.ReadFile(path)
			if err == nil {
				extractKeysFromContent(base, data, keys)
			}
		}
		return nil
	})

	return keys, err
}

func collectKeysFromFile(filePath string) (map[string]bool, error) {
	keys := make(map[string]bool)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	extractKeysFromContent(filepath.Base(filePath), data, keys)
	return keys, nil
}

func extractKeysFromContent(baseName string, data []byte, keys map[string]bool) {
	// Try Hauler Image Manifest
	var imgManifest HaulerManifest
	if err := yaml.Unmarshal(data, &imgManifest); err == nil && len(imgManifest.Spec.Images) > 0 {
		for _, img := range imgManifest.Spec.Images {
			if img.Name != "" {
				keys[img.Name] = true
				keys[normalizeImageKey(img.Name)] = true
				if img.Rewrite != "" {
					keys[img.Rewrite] = true
				}
			}
		}
		return
	}

	// Try Hauler Chart Manifest
	var chartManifest HaulerChartManifest
	if err := yaml.Unmarshal(data, &chartManifest); err == nil && chartManifest.Kind == "Charts" && len(chartManifest.Spec.Charts) > 0 {
		for _, c := range chartManifest.Spec.Charts {
			keys[fmt.Sprintf("%s-%s.tgz", c.Name, c.Version)] = true
			keys[fmt.Sprintf("%s:%s", c.Name, c.Version)] = true
			keys[fmt.Sprintf("%s/%s:%s", c.RepoURL, c.Name, c.Version)] = true
		}
		return
	}

	// Plain text lines
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys[line] = true

		if strings.HasPrefix(line, "oci::") {
			parts := strings.Split(line, "::")
			if len(parts) >= 2 {
				keys[parts[1]] = true
			}
		} else if strings.HasPrefix(line, "http::") {
			parts := strings.Split(line, "::")
			if len(parts) >= 5 {
				repo, name, ver := parts[1], parts[3], parts[4]
				keys[fmt.Sprintf("%s-%s.tgz", name, ver)] = true
				keys[fmt.Sprintf("%s:%s", name, ver)] = true
				keys[fmt.Sprintf("%s/%s:%s", repo, name, ver)] = true
			}
		} else if strings.HasPrefix(line, "local::") {
			parts := strings.Split(line, "::")
			if len(parts) >= 3 {
				keys[filepath.Base(parts[2])] = true
			}
		} else {
			keys[normalizeImageKey(line)] = true
		}
	}
}

// cmdDiff filters currentPath against baselinePath and writes delta to outPath (or stdout if empty)
func cmdDiff(baselinePath, currentPath, outPath string) {
	baselineKeys, err := collectBaselineKeys(baselinePath)
	if err != nil {
		log.Fatalf("failed to read baseline %s: %v", baselinePath, err)
	}

	currentData, err := os.ReadFile(currentPath)
	if err != nil {
		log.Fatalf("failed to read current manifest %s: %v", currentPath, err)
	}

	// Check if YAML Hauler Manifest
	if strings.HasSuffix(currentPath, ".yaml") || strings.HasSuffix(currentPath, ".yml") {
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
	}

	// Plain text diff (found-images.txt or found-charts.txt)
	diffTextManifest(currentData, baselineKeys, currentPath, outPath)
}

func diffHaulerImages(manifest HaulerManifest, baselineKeys map[string]bool, currentPath, outPath string) {
	var remaining []struct {
		Name    string `yaml:"name"`
		Rewrite string `yaml:"rewrite"`
	}

	total := len(manifest.Spec.Images)
	for _, img := range manifest.Spec.Images {
		key := img.Name
		if !baselineKeys[key] && !baselineKeys[normalizeImageKey(key)] {
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
	var remaining []struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
		RepoURL string `yaml:"repoURL"`
	}

	total := len(manifest.Spec.Charts)
	for _, c := range manifest.Spec.Charts {
		tgz := fmt.Sprintf("%s-%s.tgz", c.Name, c.Version)
		nameVer := fmt.Sprintf("%s:%s", c.Name, c.Version)
		repoNameVer := fmt.Sprintf("%s/%s:%s", c.RepoURL, c.Name, c.Version)

		if !baselineKeys[tgz] && !baselineKeys[nameVer] && !baselineKeys[repoNameVer] {
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

func diffTextManifest(data []byte, baselineKeys map[string]bool, currentPath, outPath string) {
	var deltaLines []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	total := 0

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			deltaLines = append(deltaLines, line)
			continue
		}
		total++

		if strings.HasPrefix(trimmed, "http::") {
			parts := strings.Split(trimmed, "::")
			if len(parts) >= 5 {
				repo, name, ver := parts[1], parts[3], parts[4]
				tgz := fmt.Sprintf("%s-%s.tgz", name, ver)
				nameVer := fmt.Sprintf("%s:%s", name, ver)
				repoNameVer := fmt.Sprintf("%s/%s:%s", repo, name, ver)
				if baselineKeys[trimmed] || baselineKeys[tgz] || baselineKeys[nameVer] || baselineKeys[repoNameVer] {
					continue
				}
			}
		} else if strings.HasPrefix(trimmed, "oci::") {
			parts := strings.Split(trimmed, "::")
			if len(parts) >= 2 && (baselineKeys[trimmed] || baselineKeys[parts[1]]) {
				continue
			}
		} else if strings.HasPrefix(trimmed, "local::") {
			parts := strings.Split(trimmed, "::")
			if len(parts) >= 3 && (baselineKeys[trimmed] || baselineKeys[filepath.Base(parts[2])]) {
				continue
			}
		} else {
			if baselineKeys[trimmed] || baselineKeys[normalizeImageKey(trimmed)] {
				continue
			}
		}

		deltaLines = append(deltaLines, line)
	}

	outContent := strings.Join(deltaLines, "\n")
	if len(deltaLines) > 0 {
		outContent += "\n"
	}

	deltaCount := 0
	for _, l := range deltaLines {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			deltaCount++
		}
	}

	writeDiffOutput([]byte(outContent), currentPath, outPath, total, deltaCount)
}

func writeDiffOutput(data []byte, currentPath, outPath string, total, deltaCount int) {
	log.Printf("diff %s: %d total items -> %d missing from baseline", filepath.Base(currentPath), total, deltaCount)

	if outPath == "" {
		fmt.Print(string(data))
		return
	}

	dest := outPath
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Fatalf("failed to write output %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		log.Fatalf("failed to rename output to %s: %v", dest, err)
	}
}
