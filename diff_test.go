package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffTextImages(t *testing.T) {
	dir := t.TempDir()

	baseFile := filepath.Join(dir, "base-images.txt")
	currFile := filepath.Join(dir, "curr-images.txt")
	outFile := filepath.Join(dir, "diff-images.txt")

	baseContent := "nginx:alpine\nredis:alpine\nubuntu:24.04\n"
	currContent := "nginx:alpine\nredis:alpine\npostgres:17-alpine\npython:3.13-alpine\n"

	if err := os.WriteFile(baseFile, []byte(baseContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currFile, []byte(currContent), 0644); err != nil {
		t.Fatal(err)
	}

	cmdDiff(baseFile, currFile, outFile)

	outBytes, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}

	outStr := string(outBytes)
	if strings.Contains(outStr, "nginx:alpine") || strings.Contains(outStr, "redis:alpine") {
		t.Errorf("expected common images to be removed, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "postgres:17-alpine") || !strings.Contains(outStr, "python:3.13-alpine") {
		t.Errorf("expected delta images to be present, got:\n%s", outStr)
	}
}

func TestDiffFoundCharts(t *testing.T) {
	dir := t.TempDir()

	baseFile := filepath.Join(dir, "base-charts.txt")
	currFile := filepath.Join(dir, "curr-charts.txt")
	outFile := filepath.Join(dir, "diff-charts.txt")

	baseContent := "http::my-repo::https://example.com/helm::my-chart::1.0.0\noci::registry.example.com/charts/my-oci:2.0.0\n"
	currContent := "http::my-repo::https://example.com/helm::my-chart::1.0.0\nhttp::my-repo::https://example.com/helm::new-chart::2.1.0\noci::registry.example.com/charts/my-oci:2.0.0\noci::registry.example.com/charts/new-oci:3.0.0\n"

	if err := os.WriteFile(baseFile, []byte(baseContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currFile, []byte(currContent), 0644); err != nil {
		t.Fatal(err)
	}

	cmdDiff(baseFile, currFile, outFile)

	outBytes, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}

	outStr := string(outBytes)
	if strings.Contains(outStr, "my-chart::1.0.0") || strings.Contains(outStr, "my-oci:2.0.0") {
		t.Errorf("expected baseline charts to be removed, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "new-chart::2.1.0") || !strings.Contains(outStr, "new-oci:3.0.0") {
		t.Errorf("expected new charts to be present, got:\n%s", outStr)
	}
}

func TestDiffFromZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "baseline.zip")

	// Create baseline zip
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create("resolute-installer/sidecar-charts/found-images.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("ubuntu:24.04\nnginx:alpine\n"))
	zw.Close()
	zf.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "found-images.diff.txt")
	if err := os.WriteFile(currFile, []byte("ubuntu:24.04\nnginx:alpine\nredis:alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmdDiff(zipPath, currFile, outFile)

	outBytes, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}

	outStr := strings.TrimSpace(string(outBytes))
	if outStr != "redis:alpine" {
		t.Errorf("got %q, want redis:alpine", outStr)
	}
}

func TestDiffFromTarGz(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "baseline.tar.gz")

	tf, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(tf)
	tw := tar.NewWriter(gw)

	content := "ubuntu:24.04\nnginx:alpine\n"
	if err := tw.WriteHeader(&tar.Header{
		Name: "resolute-installer/sidecar-charts/found-images.txt",
		Size: int64(len(content)),
		Mode: 0644,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gw.Close()
	tf.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "found-images.diff.txt")
	if err := os.WriteFile(currFile, []byte("ubuntu:24.04\nnginx:alpine\nredis:alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmdDiff(tarPath, currFile, outFile)

	outBytes, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}

	outStr := strings.TrimSpace(string(outBytes))
	if outStr != "redis:alpine" {
		t.Errorf("got %q, want redis:alpine", outStr)
	}
}

func TestDiffFromHTTPTarGz(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "remote-baseline.tar.gz")

	tf, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(tf)
	tw := tar.NewWriter(gw)

	content := "ubuntu:24.04\nnginx:alpine\n"
	if err := tw.WriteHeader(&tar.Header{
		Name: "resolute-installer/sidecar-charts/found-images.txt",
		Size: int64(len(content)),
		Mode: 0644,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content))
	tw.Close()
	gw.Close()
	tf.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, tarPath)
	}))
	defer ts.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "found-images.diff.txt")
	if err := os.WriteFile(currFile, []byte("ubuntu:24.04\nnginx:alpine\ngolang:1.24-alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmdDiff(ts.URL+"/remote-baseline.tar.gz", currFile, outFile)

	outBytes, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}

	outStr := strings.TrimSpace(string(outBytes))
	if outStr != "golang:1.24-alpine" {
		t.Errorf("got %q, want golang:1.24-alpine", outStr)
	}
}

func TestDiffFromHTTPZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "remote-baseline.zip")

	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create("resolute-installer/sidecar-charts/found-images.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("ubuntu:24.04\nnginx:alpine\n"))
	zw.Close()
	zf.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, zipPath)
	}))
	defer ts.Close()

	currFile := filepath.Join(dir, "found-images.txt")
	outFile := filepath.Join(dir, "found-images.diff.txt")
	if err := os.WriteFile(currFile, []byte("ubuntu:24.04\nnginx:alpine\npostgres:17-alpine\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmdDiff(ts.URL+"/remote-baseline.zip", currFile, outFile)

	outBytes, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}

	outStr := strings.TrimSpace(string(outBytes))
	if outStr != "postgres:17-alpine" {
		t.Errorf("got %q, want postgres:17-alpine", outStr)
	}
}
