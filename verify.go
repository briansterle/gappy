package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

func cmdVerify(storeDir string) {
	blobsDir := filepath.Join(storeDir, "blobs", "sha256")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		log.Fatalf("read blobs dir: %v", err)
	}

	total, corrupt := 0, 0
	fmt.Printf("verifying %s\n", storeDir)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		total++
		path := filepath.Join(blobsDir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			fmt.Printf("  ERROR    %s: %v\n", e.Name(), err)
			corrupt++
			continue
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			fmt.Printf("  ERROR    %s: %v\n", e.Name(), err)
			corrupt++
			continue
		}
		actual := hex.EncodeToString(h.Sum(nil))
		if actual != e.Name() {
			fmt.Printf("  CORRUPT  %s  (computed %s)\n", e.Name(), actual)
			corrupt++
		}
		if total%50 == 0 {
			fmt.Printf("  checked %d blobs...\n", total)
		}
	}

	// Phase 2: walk index.json to find referenced and missing blobs.
	idxPath := filepath.Join(storeDir, "index.json")
	idxData, err := os.ReadFile(idxPath)
	if err != nil {
		log.Fatalf("read index.json: %v", err)
	}
	var storeIdx ociIndexJSON
	if err := json.Unmarshal(idxData, &storeIdx); err != nil {
		log.Fatalf("parse index.json: %v", err)
	}

	referenced := make(blobMap) // reuses split.go's blobMap + collectBlobsInto
	for _, desc := range storeIdx.Manifests {
		if err := collectBlobsInto(storeDir, desc, referenced); err != nil {
			fmt.Printf("  WARNING  cannot enumerate %s: %v\n", splitRef(desc), err)
		}
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			present[e.Name()] = true
		}
	}

	missing := 0
	for hexDigest := range referenced {
		if !present[hexDigest] {
			fmt.Printf("  MISSING  sha256:%s\n", hexDigest)
			missing++
		}
	}

	orphaned := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := referenced[e.Name()]; !ok {
			orphaned++
		}
	}

	fmt.Printf("\n  %d blobs  %d corrupt  %d missing  %d orphaned\n\n",
		total, corrupt, missing, orphaned)

	if corrupt > 0 || missing > 0 {
		fmt.Println("FAIL")
		os.Exit(1)
	}
	fmt.Println("ok")
}
