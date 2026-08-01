package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Rated capacities minus ~5 % for UDF filesystem overhead.
var discPresets = map[string]int64{
	"dvd":   4_400_000_000,   // DVD-5  (4.7 GB rated)
	"dvd9":  8_075_000_000,   // DVD-9  (8.5 GB rated)
	"bd25":  23_750_000_000,  // BD-25  single-layer Blu-ray
	"bd50":  47_500_000_000,  // BD-50  dual-layer Blu-ray
	"bd100": 95_000_000_000,  // BD-100 BDXL
}

func parseSplitSize(s string) (int64, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	if n, ok := discPresets[lower]; ok {
		return n, nil
	}
	for _, p := range []struct {
		suf  string
		mult int64
	}{
		{"gib", 1 << 30},
		{"mib", 1 << 20},
		{"tib", 1 << 40},
		{"gb", 1_000_000_000},
		{"mb", 1_000_000},
		{"tb", 1_000_000_000_000},
		{"g", 1_000_000_000},
		{"m", 1_000_000},
		{"t", 1_000_000_000_000},
	} {
		if strings.HasSuffix(lower, p.suf) {
			f, err := strconv.ParseFloat(strings.TrimSuffix(lower, p.suf), 64)
			if err != nil {
				return 0, fmt.Errorf("invalid number in %q", s)
			}
			return int64(f * float64(p.mult)), nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

const refAnnotationKey = "org.opencontainers.image.ref.name"

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// blobMap maps sha256 hex digest → byte size.
type blobMap map[string]int64

type discVol struct {
	descs []v1.Descriptor
	blobs blobMap
	used  int64
}

// marginalCost is the extra bytes needed to place newBlobs on this disc
// (blobs already present are free).
func (dv *discVol) marginalCost(newBlobs blobMap) int64 {
	extra := int64(0)
	for h, sz := range newBlobs {
		if _, ok := dv.blobs[h]; !ok {
			extra += sz
		}
	}
	return extra
}

func (dv *discVol) place(desc v1.Descriptor, newBlobs blobMap) {
	extra := dv.marginalCost(newBlobs)
	dv.descs = append(dv.descs, desc)
	for h, sz := range newBlobs {
		dv.blobs[h] = sz
	}
	dv.used += extra
}

// collectBlobsInto walks an OCI descriptor recursively and records every
// referenced blob (manifest, config, layers) into out.
func collectBlobsInto(storeDir string, desc v1.Descriptor, out blobMap) error {
	hex := desc.Digest.Hex
	blobPath := filepath.Join(storeDir, "blobs", "sha256", hex)

	info, err := os.Stat(blobPath)
	if err != nil {
		return nil // missing blob — skip gracefully
	}
	out[hex] = info.Size()

	data, err := os.ReadFile(blobPath)
	if err != nil {
		return err
	}

	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		var idx struct {
			Manifests []v1.Descriptor `json:"manifests"`
		}
		if err := json.Unmarshal(data, &idx); err != nil {
			return fmt.Errorf("parse index %s: %w", hex[:12], err)
		}
		for _, child := range idx.Manifests {
			if err := collectBlobsInto(storeDir, child, out); err != nil {
				return err
			}
		}
	default:
		var mf struct {
			Config v1.Descriptor   `json:"config"`
			Layers []v1.Descriptor `json:"layers"`
		}
		if err := json.Unmarshal(data, &mf); err != nil {
			return fmt.Errorf("parse manifest %s: %w", hex[:12], err)
		}
		all := append([]v1.Descriptor{mf.Config}, mf.Layers...)
		for _, d := range all {
			p := filepath.Join(storeDir, "blobs", "sha256", d.Digest.Hex)
			if fi, err := os.Stat(p); err == nil {
				out[d.Digest.Hex] = fi.Size()
			}
		}
	}
	return nil
}

func splitRef(desc v1.Descriptor) string {
	if desc.Annotations != nil {
		if r := desc.Annotations[refAnnotationKey]; r != "" {
			return r
		}
	}
	return desc.Digest.String()[:19]
}

// ociIndexJSON is a minimal OCI image index for JSON marshalling.
type ociIndexJSON struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []v1.Descriptor `json:"manifests"`
}

// loadReferencedBlobs reads storeDir's index.json and walks every top-level
// manifest to build the set of blobs still reachable from the store. warn, if
// non-nil, is called once per manifest that can't be enumerated (e.g. a
// missing or corrupt blob) rather than treating it as fatal.
func loadReferencedBlobs(storeDir string, warn func(desc v1.Descriptor, err error)) (blobMap, error) {
	idxData, err := os.ReadFile(filepath.Join(storeDir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("read index.json: %w", err)
	}
	var storeIdx ociIndexJSON
	if err := json.Unmarshal(idxData, &storeIdx); err != nil {
		return nil, fmt.Errorf("parse index.json: %w", err)
	}

	referenced := make(blobMap)
	for _, d := range storeIdx.Manifests {
		if err := collectBlobsInto(storeDir, d, referenced); err != nil && warn != nil {
			warn(d, err)
		}
	}
	return referenced, nil
}

func writeDisc(storeDir, discDir string, vol *discVol, discIdx, total int) {
	blobsDir := filepath.Join(discDir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", blobsDir, err)
	}
	if err := os.WriteFile(filepath.Join(discDir, "oci-layout"),
		[]byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"), 0644); err != nil {
		log.Fatalf("write oci-layout: %v", err)
	}

	for hex := range vol.blobs {
		src := filepath.Join(storeDir, "blobs", "sha256", hex)
		dst := filepath.Join(blobsDir, hex)
		if err := os.Link(src, dst); err != nil {
			if err2 := blobCopy(src, dst); err2 != nil {
				log.Fatalf("copy blob %s: %v", hex[:12], err2)
			}
		}
	}

	idx := ociIndexJSON{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     vol.descs,
	}
	idxData, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(discDir, "index.json"), idxData, 0644); err != nil {
		log.Fatalf("write index.json: %v", err)
	}

	refs := make([]string, 0, len(vol.descs))
	for _, d := range vol.descs {
		refs = append(refs, splitRef(d))
	}
	type discMeta struct {
		Total  int      `json:"total"`
		Index  int      `json:"index"`
		Bytes  int64    `json:"bytes"`
		Images []string `json:"images"`
	}
	metaData, _ := json.MarshalIndent(discMeta{
		Total:  total,
		Index:  discIdx,
		Bytes:  vol.used,
		Images: refs,
	}, "", "  ")
	_ = os.WriteFile(filepath.Join(discDir, ".gappy-disc"), metaData, 0644)
}

func blobCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func cmdSplit(format, storeDir, outDir string) {
	discSize, err := parseSplitSize(format)
	if err != nil {
		log.Fatalf("unknown disc format %q — use dvd, dvd9, bd25, bd50, bd100, or a size like 4.7GB", format)
	}

	idxData, err := os.ReadFile(filepath.Join(storeDir, "index.json"))
	if err != nil {
		log.Fatalf("read store index: %v", err)
	}
	var storeIdx ociIndexJSON
	if err := json.Unmarshal(idxData, &storeIdx); err != nil {
		log.Fatalf("parse store index: %v", err)
	}
	if len(storeIdx.Manifests) == 0 {
		log.Fatal("store is empty — nothing to split")
	}

	type entry struct {
		desc  v1.Descriptor
		blobs blobMap
		total int64
	}
	entries := make([]entry, 0, len(storeIdx.Manifests))
	for _, desc := range storeIdx.Manifests {
		blobs := make(blobMap)
		if err := collectBlobsInto(storeDir, desc, blobs); err != nil {
			log.Fatalf("enumerate blobs for %s: %v", splitRef(desc), err)
		}
		total := int64(0)
		for _, sz := range blobs {
			total += sz
		}
		if total > discSize {
			fmt.Printf("WARNING: %s (%.2f GB) exceeds disc capacity (%.2f GB) — it will get its own disc\n",
				splitRef(desc), float64(total)/1e9, float64(discSize)/1e9)
		}
		entries = append(entries, entry{desc, blobs, total})
	}

	// First-fit decreasing bin packing with shared-blob awareness.
	sort.Slice(entries, func(i, j int) bool { return entries[i].total > entries[j].total })

	var vols []*discVol
	for _, e := range entries {
		placed := false
		for _, vol := range vols {
			if vol.marginalCost(e.blobs)+vol.used <= discSize {
				vol.place(e.desc, e.blobs)
				placed = true
				break
			}
		}
		if !placed {
			vol := &discVol{blobs: make(blobMap)}
			vol.place(e.desc, e.blobs)
			vols = append(vols, vol)
		}
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", outDir, err)
	}

	fmt.Printf("splitting %d image(s) across %d disc(s) [%s / %.2f GB each]\n\n",
		len(entries), len(vols), strings.ToUpper(format), float64(discSize)/1e9)

	for i, vol := range vols {
		discDir := filepath.Join(outDir, fmt.Sprintf("disc-%03d", i+1))
		writeDisc(storeDir, discDir, vol, i+1, len(vols))
		refs := make([]string, 0, len(vol.descs))
		for _, d := range vol.descs {
			refs = append(refs, splitRef(d))
		}
		fmt.Printf("  disc-%03d  %5.2f GB  %s\n",
			i+1, float64(vol.used)/1e9, strings.Join(refs, ", "))
	}

	fmt.Printf("\nburn each disc-%03d/ directory to a disc.\n", 1)
	if len(vols) > 1 {
		fmt.Printf("to rejoin after transport: gappy join <out-store> %s/disc-*\n", outDir)
	}
}

func cmdJoin(outDir string, discDirs []string) {
	if err := os.MkdirAll(filepath.Join(outDir, "blobs", "sha256"), 0755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "oci-layout"),
		[]byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"), 0644); err != nil {
		log.Fatalf("write oci-layout: %v", err)
	}

	seenBlob := make(map[string]bool)
	seenDesc := make(map[string]bool)
	var allDescs []v1.Descriptor

	for _, discDir := range discDirs {
		raw, err := os.ReadFile(filepath.Join(discDir, "index.json"))
		if err != nil {
			log.Fatalf("read %s/index.json: %v", discDir, err)
		}
		var idx ociIndexJSON
		if err := json.Unmarshal(raw, &idx); err != nil {
			log.Fatalf("parse %s/index.json: %v", discDir, err)
		}
		for _, d := range idx.Manifests {
			key := d.Digest.String()
			if !seenDesc[key] {
				seenDesc[key] = true
				allDescs = append(allDescs, d)
			}
		}

		srcBlobs := filepath.Join(discDir, "blobs", "sha256")
		dstBlobs := filepath.Join(outDir, "blobs", "sha256")
		entries, err := os.ReadDir(srcBlobs)
		if err != nil {
			log.Fatalf("read blobs from %s: %v", discDir, err)
		}
		copied := 0
		for _, e := range entries {
			if seenBlob[e.Name()] {
				continue
			}
			seenBlob[e.Name()] = true
			src := filepath.Join(srcBlobs, e.Name())
			dst := filepath.Join(dstBlobs, e.Name())
			if err := os.Link(src, dst); err != nil {
				if err2 := blobCopy(src, dst); err2 != nil {
					log.Fatalf("join blob %s: %v", e.Name()[:12], err2)
				}
			}
			copied++
		}
		fmt.Printf("  %s  %d image(s)  +%d blob(s)\n", discDir, len(idx.Manifests), copied)
	}

	merged := ociIndexJSON{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     allDescs,
	}
	data, _ := json.MarshalIndent(merged, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "index.json"), data, 0644); err != nil {
		log.Fatalf("write index.json: %v", err)
	}
	fmt.Printf("\nmerged → %s  (%d image(s) total)\n", outDir, len(allDescs))
}
