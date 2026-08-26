package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// --- index and blobs -------------------------------------------------------

const refAnnotationKey = "org.opencontainers.image.ref.name"

// blobMap maps sha256 hex digest → byte size.
type blobMap map[string]int64

// ociIndexJSON is a minimal OCI image index for JSON marshalling.
type ociIndexJSON struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []v1.Descriptor `json:"manifests"`
}

// readStoreIndex loads and parses storeDir/index.json.
func readStoreIndex(storeDir string) (ociIndexJSON, error) {
	var idx ociIndexJSON
	path := filepath.Join(storeDir, "index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return idx, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return idx, fmt.Errorf("parse %s: %w", path, err)
	}
	return idx, nil
}

// writeStoreIndex writes descs as storeDir/index.json, staging through a temp
// file so an interrupted write can't leave the store without an index.
func writeStoreIndex(storeDir string, descs []v1.Descriptor) error {
	data, err := json.MarshalIndent(ociIndexJSON{
		SchemaVersion: 2,
		MediaType:     string(types.OCIImageIndex),
		Manifests:     descs,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(storeDir, ".index.json.tmp")
	if err := os.WriteFile(tmp, data, perm.file); err != nil {
		return err
	}
	if err := perm.chmodFile(tmp); err != nil { // os.WriteFile is umask-filtered
		return err
	}
	if err := os.Rename(tmp, filepath.Join(storeDir, "index.json")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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

// writeStoreFile writes a small store file at the policy's mode, chmod'ing
// past the umask that os.WriteFile is subject to.
func writeStoreFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, perm.file); err != nil {
		return err
	}
	return perm.chmodFile(path)
}

// loadReferencedBlobs reads storeDir's index.json and walks every top-level
// manifest to build the set of blobs still reachable from the store. warn, if
// non-nil, is called once per manifest that can't be enumerated (e.g. a
// missing or corrupt blob) rather than treating it as fatal.
func loadReferencedBlobs(storeDir string, warn func(desc v1.Descriptor, err error)) (blobMap, error) {
	storeIdx, err := readStoreIndex(storeDir)
	if err != nil {
		return nil, err
	}

	referenced := make(blobMap)
	for _, d := range storeIdx.Manifests {
		if err := collectBlobsInto(storeDir, d, referenced); err != nil && warn != nil {
			warn(d, err)
		}
	}
	return referenced, nil
}

func splitRef(desc v1.Descriptor) string {
	if desc.Annotations != nil {
		if r := desc.Annotations[refAnnotationKey]; r != "" {
			return r
		}
	}
	return shortDigest(desc.Digest)
}

// shortDigest renders a digest as sha256:<first 12 hex chars>.
func shortDigest(h v1.Hash) string {
	s := h.String()
	if len(s) > 19 {
		return s[:19]
	}
	return s
}

// linkOrCopy hardlinks src to dst, falling back to a byte copy when the two
// live on different filesystems — or when the link would carry the wrong mode
// and we can't fix it. A hardlink shares the source's inode, so a blob written
// 0600 by another user stays 0600 in this store too, and only its owner can
// chmod it; copying instead yields a file this user owns at the right mode.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		fi, err := os.Stat(dst)
		if err == nil && fi.Mode().Perm() == perm.file.Perm() {
			return nil
		}
		if err == nil && perm.chmodFile(dst) == nil {
			return nil
		}
		os.Remove(dst)
	}
	return blobCopy(src, dst)
}

// blobCopy copies src to dst through a temp file, so an interrupted copy can't
// leave a truncated blob sitting under a digest that then looks present.
func blobCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".blob-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := perm.chmodFile(tmp.Name()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

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

// --- permissions -----------------------------------------------------------

// storePerm is one store's permission policy, read from the store directory
// itself. An admin runs `chmod 2775 /projects/foobar/store` once and every
// writer matches it, whatever their personal umask — which matters because a
// single `pack` run by someone with umask 077 otherwise leaves a store nobody
// else can read, and on root-squashed NFS nobody can chown their way out.
type storePerm struct {
	dir  os.FileMode // e.g. 0755, or 2775 for a group-writable shared store
	file os.FileMode // dir without the execute and setgid/setuid/sticky bits
}

// defaultStorePerm applies when the store directory doesn't exist yet.
var defaultStorePerm = storePerm{dir: 0o755, file: 0o644}

// perm is the active policy, set once per command by useStore. One gappy
// invocation writes one store, so this is a global rather than a parameter
// threaded through every blob write.
var perm = defaultStorePerm

// permFromDir derives a policy from a directory's mode. Files drop the execute
// bits, and the setgid/setuid/sticky bits along with them — those mean
// something else entirely on a regular file: 2775 → 0664, 0755 → 0644.
func permFromDir(m os.FileMode) storePerm {
	return storePerm{
		dir:  m & (os.ModePerm | os.ModeSetgid | os.ModeSetuid | os.ModeSticky),
		file: m.Perm() & 0o666,
	}
}

// octal renders a mode the way chmod states it, setgid/setuid/sticky included —
// os.FileMode keeps those outside the low 9 bits, and for a shared store the
// setgid bit is the whole point, so printing 0775 for a 2775 dir hides it.
func octal(m os.FileMode) string {
	o := uint32(m.Perm())
	for _, b := range []struct {
		flag os.FileMode
		bit  uint32
	}{{os.ModeSetuid, 0o4000}, {os.ModeSetgid, 0o2000}, {os.ModeSticky, 0o1000}} {
		if m&b.flag != 0 {
			o |= b.bit
		}
	}
	return fmt.Sprintf("%04o", o)
}

// umask is the process mask that lands third-party writes on this policy:
// 0755 → 022, 2775 → 002.
func (p storePerm) umask() int { return int(^p.dir.Perm()) & 0o777 }

// shutsOthersOut reports whether the policy denies both group and other read —
// the 0700 case that strands everyone but the owner.
func (p storePerm) shutsOthersOut() bool { return p.dir.Perm()&0o044 == 0 }

// chmodFile forces path to the policy's file mode. os.Chmod ignores the umask,
// so this also corrects files made by os.CreateTemp, which is always 0600.
func (p storePerm) chmodFile(path string) error { return os.Chmod(path, p.file) }

// mkdirAll creates dir and forces the policy mode on the leaf, so a setgid bit
// survives the umask.
func (p storePerm) mkdirAll(dir string) error {
	if err := os.MkdirAll(dir, p.dir); err != nil {
		return err
	}
	return os.Chmod(dir, p.dir)
}

// normalizeStoreFiles fixes the modes of the store files gappy doesn't write
// itself. go-containerregistry passes os.ModePerm to os.WriteFile for
// index.json and oci-layout, so they land executable and umask-dependent.
func normalizeStoreFiles(storeDir string) {
	for _, name := range []string{"index.json", "oci-layout"} {
		path := filepath.Join(storeDir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := perm.chmodFile(path); err != nil {
			log.Printf("warning: chmod %s: %v", path, err)
		}
	}
	if err := perm.mkdirAll(filepath.Join(storeDir, "blobs", "sha256")); err != nil {
		log.Printf("warning: chmod blobs dir: %v", err)
	}
}

// useStore adopts storeDir's permission policy for the rest of the command.
// Every write path — gappy's own and go-containerregistry's — follows from it.
func useStore(storeDir string) storePerm {
	if fi, err := os.Stat(storeDir); err == nil && fi.IsDir() {
		perm = permFromDir(fi.Mode())
	} else {
		perm = defaultStorePerm
	}
	setUmask(perm.umask())
	if perm.shutsOthersOut() {
		log.Printf("warning: %s is mode %s — no other user can read this store; see gappy fix-perms",
			storeDir, octal(perm.dir))
	}
	return perm
}

// --- fix-perms -------------------------------------------------------------

// permReport tallies a fix-perms walk.
type permReport struct {
	fixed, alreadyOK, failed int
	byOwner                  map[string][]string // "name (uid N)" → offending paths
}

// ownerOf labels the user owning fi, for naming exactly whose files block a
// repair.
func ownerOf(fi os.FileInfo) string {
	uid, name := fileOwner(fi)
	switch {
	case name != "" && uid != "":
		return fmt.Sprintf("%s (uid %s)", name, uid)
	case uid != "":
		return "uid " + uid
	}
	return "unknown owner"
}

// cmdFixPerms brings an existing store in line with its directory's policy.
// It can only chmod what the running user owns, so whatever is left over is
// reported grouped by owner — the people who have to run it themselves.
func cmdFixPerms(storeDir string, dryRun bool) {
	fi, err := os.Stat(storeDir)
	if err != nil {
		log.Fatalf("stat %s: %v", storeDir, err)
	}
	if !fi.IsDir() {
		log.Fatalf("%s is not a directory", storeDir)
	}
	p := permFromDir(fi.Mode())

	fmt.Printf("%s  (dirs %s, files %s — taken from the store directory)\n\n",
		storeDir, octal(p.dir), octal(p.file))
	if p.shutsOthersOut() {
		fmt.Printf("  the store directory is itself %s, so this policy keeps everyone else out.\n", octal(p.dir))
		fmt.Printf("  for a shared store run: chmod 2775 %s   (then re-run this)\n\n", storeDir)
	}

	rep := permReport{byOwner: map[string][]string{}}
	walkErr := filepath.WalkDir(storeDir, func(path string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(storeDir, path)
		if err != nil {
			// An unreadable directory is itself the problem being diagnosed —
			// name it and keep walking rather than aborting the whole store.
			fmt.Printf("  UNREADABLE  %s: %v\n", rel, err)
			rep.failed++
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		want := p.file
		if d.IsDir() {
			want = p.dir
		}
		if info.Mode()&(os.ModePerm|os.ModeSetgid|os.ModeSetuid|os.ModeSticky) == want {
			rep.alreadyOK++
			return nil
		}
		if dryRun {
			rep.fixed++
			return nil
		}
		if err := os.Chmod(path, want); err != nil {
			rep.failed++
			owner := ownerOf(info)
			rep.byOwner[owner] = append(rep.byOwner[owner], rel)
			return nil
		}
		rep.fixed++
		return nil
	})
	if walkErr != nil {
		log.Fatalf("walk %s: %v", storeDir, walkErr)
	}

	verb := "fixed"
	if dryRun {
		verb = "would fix"
	}
	fmt.Printf("  %-9s %d path(s)\n", verb, rep.fixed)
	fmt.Printf("  %-9s %d path(s) already correct\n", "ok", rep.alreadyOK)
	if rep.failed == 0 {
		if dryRun {
			fmt.Println("\ndry run — nothing changed")
		}
		return
	}

	fmt.Printf("  %-9s %d path(s) owned by someone else\n\n", "blocked", rep.failed)
	owners := make([]string, 0, len(rep.byOwner))
	for o := range rep.byOwner {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	for _, o := range owners {
		paths := rep.byOwner[o]
		fmt.Printf("    %-26s %d path(s)   e.g. %s\n", o, len(paths), paths[0])
	}
	fmt.Println("\n  only the owner can chmod their own files. ask each of them to run:")
	fmt.Printf("    gappy fix-perms %s\n", storeDir)
	os.Exit(1)
}

// --- verify ----------------------------------------------------------------

func cmdVerify(storeDir string) {
	blobsDir := filepath.Join(storeDir, "blobs", "sha256")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		log.Fatalf("read blobs dir: %v", err)
	}

	total, corrupt, unreadable := 0, 0, 0
	fmt.Printf("verifying %s\n", storeDir)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		total++
		path := filepath.Join(blobsDir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			// A blob we can't open is a permissions problem, not corruption —
			// saying "corrupt" here sends people looking for a bad transfer.
			if os.IsPermission(err) {
				owner := ""
				if fi, serr := os.Stat(path); serr == nil {
					owner = "  owned by " + ownerOf(fi)
				}
				fmt.Printf("  UNREADABLE  %s%s\n", e.Name(), owner)
				unreadable++
			} else {
				fmt.Printf("  ERROR    %s: %v\n", e.Name(), err)
				corrupt++
			}
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
	referenced, err := loadReferencedBlobs(storeDir, func(desc v1.Descriptor, err error) {
		fmt.Printf("  WARNING  cannot enumerate %s: %v\n", splitRef(desc), err)
	})
	if err != nil {
		log.Fatalf("%v", err)
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

	fmt.Printf("\n  %d blobs  %d corrupt  %d missing  %d orphaned  %d unreadable\n\n",
		total, corrupt, missing, orphaned, unreadable)

	if unreadable > 0 {
		fmt.Printf("%d blob(s) could not be read — this is a permissions problem, not a bad\n", unreadable)
		fmt.Printf("transfer. run: gappy fix-perms %s\n\n", storeDir)
	}
	if corrupt > 0 || missing > 0 || unreadable > 0 {
		fmt.Println("FAIL")
		os.Exit(1)
	}
	fmt.Println("ok")
}

// --- rmi -------------------------------------------------------------------

// cmdRmi removes an image (by ref name or digest) from the store, then
// deletes any blobs that are no longer referenced by a remaining image —
// mirroring `docker rmi`'s all-in-one semantics rather than requiring a
// separate gc step.
func cmdRmi(storeDir, ref string) {
	useStore(storeDir)
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

// --- split -----------------------------------------------------------------

// Rated capacities minus ~5 % for UDF filesystem overhead.
var discPresets = map[string]int64{
	"dvd":   4_400_000_000,  // DVD-5  (4.7 GB rated)
	"dvd9":  8_075_000_000,  // DVD-9  (8.5 GB rated)
	"bd25":  23_750_000_000, // BD-25  single-layer Blu-ray
	"bd50":  47_500_000_000, // BD-50  dual-layer Blu-ray
	"bd100": 95_000_000_000, // BD-100 BDXL
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

func writeDisc(storeDir, discDir string, vol *discVol, discIdx, total int) {
	blobsDir := filepath.Join(discDir, "blobs", "sha256")
	if err := perm.mkdirAll(blobsDir); err != nil {
		log.Fatalf("mkdir %s: %v", blobsDir, err)
	}
	if err := writeStoreFile(filepath.Join(discDir, "oci-layout"),
		[]byte("{\"imageLayoutVersion\":\"1.0.0\"}\n")); err != nil {
		log.Fatalf("write oci-layout: %v", err)
	}

	for hex := range vol.blobs {
		src := filepath.Join(storeDir, "blobs", "sha256", hex)
		if err := linkOrCopy(src, filepath.Join(blobsDir, hex)); err != nil {
			log.Fatalf("copy blob %s: %v", hex[:12], err)
		}
	}

	if err := writeStoreIndex(discDir, vol.descs); err != nil {
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
	_ = writeStoreFile(filepath.Join(discDir, ".gappy-disc"), metaData)
}

func cmdSplit(format, storeDir, outDir string) {
	useStore(storeDir)
	discSize, err := parseSplitSize(format)
	if err != nil {
		log.Fatalf("unknown disc format %q — use dvd, dvd9, bd25, bd50, bd100, or a size like 4.7GB", format)
	}

	storeIdx, err := readStoreIndex(storeDir)
	if err != nil {
		log.Fatalf("%v", err)
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

	if err := perm.mkdirAll(outDir); err != nil {
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

// --- join ------------------------------------------------------------------

func cmdJoin(outDir string, discDirs []string) {
	useStore(outDir)
	if err := perm.mkdirAll(filepath.Join(outDir, "blobs", "sha256")); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	if err := writeStoreFile(filepath.Join(outDir, "oci-layout"),
		[]byte("{\"imageLayoutVersion\":\"1.0.0\"}\n")); err != nil {
		log.Fatalf("write oci-layout: %v", err)
	}

	seenBlob := make(map[string]bool)
	seenDesc := make(map[string]bool)
	var allDescs []v1.Descriptor

	for _, discDir := range discDirs {
		idx, err := readStoreIndex(discDir)
		if err != nil {
			log.Fatalf("%v", err)
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
			if err := linkOrCopy(filepath.Join(srcBlobs, e.Name()), filepath.Join(dstBlobs, e.Name())); err != nil {
				log.Fatalf("join blob %s: %v", e.Name()[:12], err)
			}
			copied++
		}
		fmt.Printf("  %s  %d image(s)  +%d blob(s)\n", discDir, len(idx.Manifests), copied)
	}

	if err := writeStoreIndex(outDir, allDescs); err != nil {
		log.Fatalf("write index.json: %v", err)
	}
	fmt.Printf("\nmerged → %s  (%d image(s) total)\n", outDir, len(allDescs))
}

// --- merge -----------------------------------------------------------------

// mergeStats tallies one `gappy merge` run across every incremental store.
type mergeStats struct {
	added, updated, unchanged  int
	blobsAdded, blobsPresent   int
	chartsAdded, chartsPresent int
	blobBytes, chartBytes      int64
}

// extractDryRun pulls a -n/--dry-run flag out of a subcommand's positional
// args, which Go's flag package stops parsing at.
func extractDryRun(args []string) ([]string, bool) {
	rest := make([]string, 0, len(args))
	dry := false
	for _, a := range args {
		switch a {
		case "-n", "-dry-run", "--dry-run":
			dry = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, dry
}

// cmdMerge folds one or more incremental stores into baseDir in place — a left
// join, with baseDir as the side that survives and gets written to. Only the
// increments are read, so merging a 2 GB delta into a 500 GB base moves 2 GB.
//
// Blobs are content-addressed, so de-duplication is a digest check. Index
// entries de-duplicate on (ref name, digest): an entry whose ref name already
// exists in the base with a different digest replaces it, matching what `pack`
// does when a tag moves. Chart archives de-duplicate on path, matching
// `pack-charts`.
func cmdMerge(baseDir string, incDirs []string, dryRun bool) {
	useStore(baseDir)
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		log.Fatalf("resolve %s: %v", baseDir, err)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "index.json")); err != nil {
		log.Fatalf("%s is not an OCI store (no index.json) — use gappy join to build a fresh store from parts", baseDir)
	}
	baseIdx, err := readStoreIndex(baseDir)
	if err != nil {
		log.Fatal(err)
	}

	// Working copy of the base index, plus lookups for the two de-dup keys.
	descs := append([]v1.Descriptor(nil), baseIdx.Manifests...)
	byRef := make(map[string]int, len(descs))
	digests := make(map[string]bool, len(descs))
	for i, d := range descs {
		if ref := d.Annotations[refAnnotationKey]; ref != "" {
			byRef[ref] = i
		}
		digests[d.Digest.String()] = true
	}

	baseBlobs := filepath.Join(baseDir, "blobs", "sha256")
	if !dryRun {
		if err := perm.mkdirAll(baseBlobs); err != nil {
			log.Fatalf("mkdir %s: %v", baseBlobs, err)
		}
	}

	fmt.Printf("merging %d store(s) into %s\n", len(incDirs), baseDir)

	var st mergeStats
	for _, incDir := range incDirs {
		incAbs, err := filepath.Abs(incDir)
		if err != nil {
			log.Fatalf("resolve %s: %v", incDir, err)
		}
		if incAbs == baseAbs {
			log.Fatalf("%s and %s are the same store", incDir, baseDir)
		}
		fmt.Printf("\n  %s\n", incDir)
		mergeStore(baseDir, incDir, dryRun, &descs, byRef, digests, &st)
	}

	// index.json goes last, so the store never points at blobs that are still
	// in flight.
	if !dryRun {
		if err := writeStoreIndex(baseDir, descs); err != nil {
			log.Fatalf("write index.json: %v", err)
		}
	}

	fmt.Printf("\n  images  +%d added  ~%d updated  =%d unchanged\n", st.added, st.updated, st.unchanged)
	fmt.Printf("  blobs   +%d (%s)  %d already present\n",
		st.blobsAdded, humanBytes(st.blobBytes), st.blobsPresent)
	if st.chartsAdded+st.chartsPresent > 0 {
		fmt.Printf("  charts  +%d (%s)  %d already present\n",
			st.chartsAdded, humanBytes(st.chartBytes), st.chartsPresent)
	}
	if dryRun {
		fmt.Printf("\ndry run — %s not modified\n", baseDir)
		return
	}
	fmt.Printf("\nmerged → %s  (%d image(s) total)\n", baseDir, len(descs))
}

// mergeStore folds a single incremental store into the base, updating the
// in-memory index (descs/byRef/digests) and copying in whatever blobs and
// charts the base is missing.
func mergeStore(baseDir, incDir string, dryRun bool,
	descs *[]v1.Descriptor, byRef map[string]int, digests map[string]bool, st *mergeStats) {

	var incIdx ociIndexJSON
	if _, err := os.Stat(filepath.Join(incDir, "index.json")); err == nil {
		incIdx, err = readStoreIndex(incDir)
		if err != nil {
			log.Fatal(err)
		}
	} else if _, err := os.Stat(filepath.Join(incDir, "helm")); err != nil {
		log.Fatalf("%s is not a gappy store (no index.json, no helm/)", incDir)
	}

	// Walk every incremental manifest for its blobs, including ones whose index
	// entry turns out to be unchanged — that way a base missing a blob it
	// already indexes gets healed rather than staying broken.
	wanted := make(blobMap)
	for _, d := range incIdx.Manifests {
		if err := collectBlobsInto(incDir, d, wanted); err != nil {
			log.Fatalf("enumerate blobs for %s in %s: %v", splitRef(d), incDir, err)
		}

		ref := d.Annotations[refAnnotationKey]
		if ref == "" {
			// Untagged entry — its digest is the only identity it has.
			if digests[d.Digest.String()] {
				st.unchanged++
				continue
			}
			*descs = append(*descs, d)
			digests[d.Digest.String()] = true
			st.added++
			fmt.Printf("    + %-44s %s\n", splitRef(d), shortDigest(d.Digest))
			continue
		}
		if i, ok := byRef[ref]; ok {
			if (*descs)[i].Digest == d.Digest {
				st.unchanged++
				fmt.Printf("    = %s\n", ref)
				continue
			}
			fmt.Printf("    ~ %-44s %s → %s\n", ref, shortDigest((*descs)[i].Digest), shortDigest(d.Digest))
			(*descs)[i] = d
			digests[d.Digest.String()] = true
			st.updated++
			continue
		}
		byRef[ref] = len(*descs)
		*descs = append(*descs, d)
		digests[d.Digest.String()] = true
		st.added++
		fmt.Printf("    + %-44s %s\n", ref, shortDigest(d.Digest))
	}

	mergeBlobs(baseDir, incDir, wanted, dryRun, st)
	if err := mergeCharts(baseDir, incDir, dryRun, st); err != nil {
		log.Fatalf("merge charts from %s: %v", incDir, err)
	}
}

// mergeBlobs copies the blobs in wanted that the base store does not already
// hold. Blobs are content-addressed, so a digest already present is by
// definition the same bytes and is skipped.
func mergeBlobs(baseDir, incDir string, wanted blobMap, dryRun bool, st *mergeStats) {
	baseBlobs := filepath.Join(baseDir, "blobs", "sha256")

	missing := make([]string, 0, len(wanted))
	var bytes int64
	for hex, size := range wanted {
		if _, err := os.Stat(filepath.Join(baseBlobs, hex)); err == nil {
			st.blobsPresent++
			continue
		}
		missing = append(missing, hex)
		bytes += size
	}
	st.blobsAdded += len(missing)
	st.blobBytes += bytes

	if len(missing) == 0 || dryRun {
		return
	}
	fmt.Printf("    copying %d blob(s), %s\n", len(missing), humanBytes(bytes))
	for _, hex := range missing {
		src := filepath.Join(incDir, "blobs", "sha256", hex)
		if err := linkOrCopy(src, filepath.Join(baseBlobs, hex)); err != nil {
			log.Fatalf("merge blob %s: %v", hex[:12], err)
		}
	}
}

// mergeCharts copies Helm archives under incDir/helm that the base store does
// not already have. Charts are identified by their path
// (repo/name-version.tgz), so one already in the base is left alone — the same
// rule pack-charts applies when it finds a chart on disk.
func mergeCharts(baseDir, incDir string, dryRun bool, st *mergeStats) error {
	srcRoot := filepath.Join(incDir, "helm")
	if _, err := os.Stat(srcRoot); err != nil {
		return nil
	}
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(baseDir, "helm", rel)
		if _, err := os.Stat(dst); err == nil {
			st.chartsPresent++
			return nil
		}
		st.chartsAdded++
		if info, err := d.Info(); err == nil {
			st.chartBytes += info.Size()
		}
		fmt.Printf("    + helm/%s\n", filepath.ToSlash(rel))
		if dryRun {
			return nil
		}
		if err := perm.mkdirAll(filepath.Dir(dst)); err != nil {
			return err
		}
		return linkOrCopy(path, dst)
	})
}
