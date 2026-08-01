package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"
)

// cmdRmi removes an image (by ref name or digest) from the store, then
// deletes any blobs that are no longer referenced by a remaining image —
// mirroring `docker rmi`'s all-in-one semantics rather than requiring a
// separate gc step.
func cmdRmi(storeDir, ref string) {
	lyt, err := layout.FromPath(storeDir)
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}
	idx, err := lyt.ImageIndex()
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		log.Fatalf("failed to load store: %v", err)
	}

	// A ref name always identifies exactly one index entry (the store
	// replaces rather than duplicates on re-push). A digest can be shared by
	// several tags of the same image — in that case ask the user to name the
	// tag they mean rather than guessing which entries to drop.
	byDigest := false
	var matches []v1.Descriptor
	if hash, err := v1.NewHash(ref); err == nil {
		byDigest = true
		for _, d := range im.Manifests {
			if d.Digest == hash {
				matches = append(matches, d)
			}
		}
	} else {
		for _, d := range im.Manifests {
			if d.Annotations[refAnnotationKey] == ref {
				matches = append(matches, d)
			}
		}
	}

	if len(matches) == 0 {
		log.Fatalf("no such image: %s", ref)
	}
	if len(matches) > 1 {
		tags := make([]string, len(matches))
		for i, d := range matches {
			tags[i] = d.Annotations[refAnnotationKey]
		}
		log.Fatalf("%s is tagged as %s — remove one of those tags instead", ref, strings.Join(tags, ", "))
	}

	target := matches[0]
	displayRef := target.Annotations[refAnnotationKey]
	if displayRef == "" {
		displayRef = target.Digest.String()
	}

	rm := match.Name(displayRef)
	if byDigest {
		rm = match.Digests(target.Digest)
	}
	if err := lyt.RemoveDescriptors(rm); err != nil {
		log.Fatalf("failed to remove %s: %v", displayRef, err)
	}

	freed, removed, err := gcOrphanedBlobs(storeDir)
	if err != nil {
		log.Fatalf("removed %s but failed to garbage-collect blobs: %v", displayRef, err)
	}

	fmt.Printf("removed %s (%s)\n", displayRef, target.Digest)
	if removed > 0 {
		fmt.Printf("freed %s (%d blobs)\n", humanBytes(freed), removed)
	}
}

// gcOrphanedBlobs deletes every blob under storeDir that is no longer
// reachable from index.json, returning the bytes and blob count freed.
func gcOrphanedBlobs(storeDir string) (freed int64, removed int, err error) {
	referenced, err := loadReferencedBlobs(storeDir, func(d v1.Descriptor, err error) {
		log.Printf("warning: cannot enumerate %s: %v", splitRef(d), err)
	})
	if err != nil {
		return 0, 0, err
	}

	blobsDir := filepath.Join(storeDir, "blobs", "sha256")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		return 0, 0, fmt.Errorf("read blobs dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := referenced[e.Name()]; ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(blobsDir, e.Name())); err != nil {
			log.Printf("warning: failed to remove blob %s: %v", e.Name(), err)
			continue
		}
		freed += info.Size()
		removed++
	}
	return freed, removed, nil
}
