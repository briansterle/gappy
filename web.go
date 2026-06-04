package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

//go:embed web/index.html web/app.css web/app.js
var webAssets embed.FS

const refAnnotationKey = "org.opencontainers.image.ref.name"

// storeMu serializes mutations of the OCI layout's index.json. Blob writes are
// content-addressed and safe to run concurrently; only the index.json
// read-modify-write needs guarding.
var storeMu sync.Mutex

// ---------------------------------------------------------------------------
// SSE job hub
// ---------------------------------------------------------------------------

type frame struct {
	event string
	data  string
}

// job is an append-only event log that any number of SSE clients can replay
// and follow live. Frames are never dropped; late subscribers replay history.
type job struct {
	id   string
	kind string

	mu     sync.Mutex
	cond   *sync.Cond
	frames []frame
	done   bool
}

func (j *job) emit(event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprintf("%q", err.Error()))
	}
	j.mu.Lock()
	j.frames = append(j.frames, frame{event: event, data: string(b)})
	j.cond.Broadcast()
	j.mu.Unlock()
}

func (j *job) log(format string, args ...any) {
	j.emit("log", map[string]string{"msg": fmt.Sprintf(format, args...)})
}

func (j *job) finish() {
	j.mu.Lock()
	j.done = true
	j.cond.Broadcast()
	j.mu.Unlock()
}

type hub struct {
	mu   sync.Mutex
	jobs map[string]*job
	seq  int
}

func newHub() *hub { return &hub{jobs: map[string]*job{}} }

func (h *hub) create(kind string) *job {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	j := &job{id: fmt.Sprintf("%s-%d", kind, h.seq), kind: kind}
	j.cond = sync.NewCond(&j.mu)
	h.jobs[j.id] = j
	return j
}

func (h *hub) get(id string) *job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.jobs[id]
}

// ---------------------------------------------------------------------------
// progress instrumentation for PACK (pull): wrap images/indexes so every byte
// read off the wire while writing to the store is counted, per layer.
// ---------------------------------------------------------------------------

type layerProg struct {
	Index    int    `json:"i"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
	Cached   bool   `json:"cached"`
	Done     bool   `json:"done"`
}

type packSink struct {
	j     *job
	total int64
	start time.Time

	mu       sync.Mutex
	layers   []*layerProg
	byDigest map[string]*layerProg
	received int64
	prevByte int64
	prevTime time.Time
}

// add accumulates bytes for a layer. It only mutates counters — emission is
// driven by a separate ticker so a stalled transfer still reports 0 B/s and
// concurrent readers never contend on JSON marshalling.
func (s *packSink) add(digest string, n int) {
	s.mu.Lock()
	if lp := s.byDigest[digest]; lp != nil {
		lp.Received += int64(n)
		if lp.Size > 0 && lp.Received >= lp.Size {
			lp.Done = true
		}
	}
	s.received += int64(n)
	s.mu.Unlock()
}

// emit sends a progress snapshot. Safe to call from a ticker goroutine; the
// snapshot is taken under the lock and the SSE write happens outside it.
func (s *packSink) emit(done bool) {
	s.mu.Lock()
	now := time.Now()
	dt := now.Sub(s.prevTime).Seconds()
	var bps float64
	if dt > 0 {
		bps = float64(s.received-s.prevByte) / dt
	}
	s.prevByte = s.received
	s.prevTime = now
	ls := make([]map[string]any, len(s.layers))
	for i, lp := range s.layers {
		ls[i] = map[string]any{"i": lp.Index, "received": lp.Received, "done": lp.Done}
	}
	recv, total := s.received, s.total
	s.mu.Unlock()

	var eta float64
	if bps > 0 && total > recv {
		eta = float64(total-recv) / bps
	}
	s.j.emit("progress", map[string]any{
		"received": recv,
		"total":    total,
		"bps":      int64(bps),
		"etaSec":   eta,
		"layers":   ls,
		"done":     done,
	})
}

// ticker periodically emits progress until stop is closed, then emits a final
// snapshot. Returns when the goroutine has fully stopped.
func runTicker(emit func(done bool), stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				emit(false)
			}
		}
	}()
	return done
}

// countingReadCloser counts bytes as they are read.
type countingReadCloser struct {
	rc     io.ReadCloser
	onRead func(n int)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.onRead(n)
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

type progressLayer struct {
	v1.Layer
	onRead func(n int)
}

func (l progressLayer) Compressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	return &countingReadCloser{rc: rc, onRead: l.onRead}, nil
}

type progressImage struct {
	v1.Image
	sink *packSink
}

func (p progressImage) wrap(l v1.Layer) (v1.Layer, error) {
	d, err := l.Digest()
	if err != nil {
		return l, nil // can't key it; pass through uncounted rather than fail
	}
	ds := d.String()
	return progressLayer{Layer: l, onRead: func(n int) { p.sink.add(ds, n) }}, nil
}

func (p progressImage) Layers() ([]v1.Layer, error) {
	ls, err := p.Image.Layers()
	if err != nil {
		return nil, err
	}
	out := make([]v1.Layer, len(ls))
	for i, l := range ls {
		if out[i], err = p.wrap(l); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (p progressImage) LayerByDigest(h v1.Hash) (v1.Layer, error) {
	l, err := p.Image.LayerByDigest(h)
	if err != nil {
		return nil, err
	}
	return p.wrap(l)
}

// progressIndex wraps a v1.ImageIndex so child images (every platform) are
// themselves wrapped, propagating byte counting through the whole tree. It uses
// a named field rather than embedding because v1.ImageIndex itself declares an
// ImageIndex method, which would collide with an embedded field of that name.
type progressIndex struct {
	idx  v1.ImageIndex
	sink *packSink
}

func (p progressIndex) MediaType() (types.MediaType, error)       { return p.idx.MediaType() }
func (p progressIndex) Digest() (v1.Hash, error)                  { return p.idx.Digest() }
func (p progressIndex) Size() (int64, error)                      { return p.idx.Size() }
func (p progressIndex) IndexManifest() (*v1.IndexManifest, error) { return p.idx.IndexManifest() }
func (p progressIndex) RawManifest() ([]byte, error)              { return p.idx.RawManifest() }

func (p progressIndex) Image(h v1.Hash) (v1.Image, error) {
	img, err := p.idx.Image(h)
	if err != nil {
		return nil, err
	}
	return progressImage{Image: img, sink: p.sink}, nil
}

func (p progressIndex) ImageIndex(h v1.Hash) (v1.ImageIndex, error) {
	cidx, err := p.idx.ImageIndex(h)
	if err != nil {
		return nil, err
	}
	return progressIndex{idx: cidx, sink: p.sink}, nil
}

// Layer forwards the optional withLayer interface that layout.WriteIndex relies
// on to persist non-image child manifests (e.g. attestation blobs). Passed
// through uncounted so progress totals stay tied to image layers.
func (p progressIndex) Layer(h v1.Hash) (v1.Layer, error) {
	if wl, ok := p.idx.(interface {
		Layer(v1.Hash) (v1.Layer, error)
	}); ok {
		return wl.Layer(h)
	}
	return nil, fmt.Errorf("layer %s not available", h)
}

// ---------------------------------------------------------------------------
// store enumeration (browse)
// ---------------------------------------------------------------------------

type imageInfo struct {
	Ref        string   `json:"ref"`
	Digest     string   `json:"digest"`
	MediaType  string   `json:"mediaType"`
	Size       int64    `json:"size"`
	IsIndex    bool     `json:"isIndex"`
	Platforms  []string `json:"platforms"`
	LayerCount int      `json:"layerCount"`
}

func imageBytes(img v1.Image) (size int64, layers int) {
	m, err := img.Manifest()
	if err != nil {
		return 0, 0
	}
	size = m.Config.Size
	for _, l := range m.Layers {
		size += l.Size
	}
	return size, len(m.Layers)
}

func enumerateStore(storeDir string) ([]imageInfo, error) {
	if _, err := os.Stat(filepath.Join(storeDir, "index.json")); err != nil {
		return []imageInfo{}, nil // no store yet
	}
	idx, err := layout.ImageIndexFromPath(storeDir)
	if err != nil {
		return nil, err
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}

	out := []imageInfo{}
	for _, desc := range im.Manifests {
		info := imageInfo{
			Ref:       desc.Annotations[refAnnotationKey],
			Digest:    desc.Digest.String(),
			MediaType: string(desc.MediaType),
			Platforms: []string{},
		}
		if info.Ref == "" {
			info.Ref = desc.Digest.String()
		}

		switch {
		case desc.MediaType.IsIndex():
			info.IsIndex = true
			child, err := idx.ImageIndex(desc.Digest)
			if err != nil {
				break
			}
			cm, err := child.IndexManifest()
			if err != nil {
				break
			}
			seen := map[string]bool{}
			for _, m := range cm.Manifests {
				if m.Platform != nil && m.Platform.Architecture != "" && m.Platform.Architecture != "unknown" {
					info.Platforms = append(info.Platforms, m.Platform.String())
				}
				if !m.MediaType.IsImage() {
					continue
				}
				cimg, err := child.Image(m.Digest)
				if err != nil {
					continue
				}
				cmf, err := cimg.Manifest()
				if err != nil {
					continue
				}
				info.Size += cmf.Config.Size
				for _, l := range cmf.Layers {
					if !seen[l.Digest.String()] {
						seen[l.Digest.String()] = true
						info.Size += l.Size
					}
				}
				info.LayerCount += len(cmf.Layers)
			}
		case desc.MediaType.IsImage():
			img, err := idx.Image(desc.Digest)
			if err != nil {
				break
			}
			info.Size, info.LayerCount = imageBytes(img)
			if cf, err := img.ConfigFile(); err == nil && cf != nil && cf.OS != "" {
				info.Platforms = append(info.Platforms, fmt.Sprintf("%s/%s", cf.OS, cf.Architecture))
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func isCached(storeDir, digest string, size int64) bool {
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return false
	}
	fi, err := os.Stat(filepath.Join(storeDir, "blobs", parts[0], parts[1]))
	return err == nil && fi.Size() == size
}

// ---------------------------------------------------------------------------
// server
// ---------------------------------------------------------------------------

type server struct {
	storeDir    string
	regBindAddr string        // address the unpack registry binds; "" → 127.0.0.1:5000
	remoteAuth  remote.Option // auth for upstream pulls; overridable in tests
	hub         *hub

	regMu      sync.Mutex
	regStarted bool
	regAddr    string
}

func newServer(storeDir string) *server {
	return &server{
		storeDir:    storeDir,
		regBindAddr: "127.0.0.1:5000",
		remoteAuth:  makeRemoteAuthOption(),
		hub:         newHub(),
	}
}

func openStore(storeDir string) (layout.Path, error) {
	if _, err := os.Stat(filepath.Join(storeDir, "index.json")); os.IsNotExist(err) {
		return layout.Write(storeDir, empty.Index)
	}
	return layout.FromPath(storeDir)
}

// ---- PACK ----------------------------------------------------------------

type layerSeed struct {
	digest string
	size   int64
	cached bool
}

// gatherLayers fetches manifests (cheap) to enumerate the deduped layer set and
// total bytes for a reference, plus platform labels.
func gatherLayers(s *server, desc *remote.Descriptor) (seeds []layerSeed, platforms []string, isIndex bool, err error) {
	platforms = []string{}
	if desc.MediaType.IsIndex() {
		isIndex = true
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, nil, true, err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, nil, true, err
		}
		seen := map[string]bool{}
		for _, m := range im.Manifests {
			if m.Platform != nil && m.Platform.Architecture != "" && m.Platform.Architecture != "unknown" {
				platforms = append(platforms, m.Platform.String())
			}
			if !m.MediaType.IsImage() {
				continue
			}
			img, err := idx.Image(m.Digest)
			if err != nil {
				continue
			}
			mf, err := img.Manifest()
			if err != nil {
				continue
			}
			for _, l := range mf.Layers {
				ds := l.Digest.String()
				if seen[ds] {
					continue
				}
				seen[ds] = true
				seeds = append(seeds, layerSeed{digest: ds, size: l.Size, cached: isCached(s.storeDir, ds, l.Size)})
			}
		}
		return seeds, platforms, true, nil
	}

	img, err := desc.Image()
	if err != nil {
		return nil, nil, false, err
	}
	mf, err := img.Manifest()
	if err != nil {
		return nil, nil, false, err
	}
	for _, l := range mf.Layers {
		ds := l.Digest.String()
		seeds = append(seeds, layerSeed{digest: ds, size: l.Size, cached: isCached(s.storeDir, ds, l.Size)})
	}
	if cf, err := img.ConfigFile(); err == nil && cf != nil && cf.OS != "" {
		platforms = append(platforms, fmt.Sprintf("%s/%s", cf.OS, cf.Architecture))
	}
	return seeds, platforms, false, nil
}

func (s *server) runPack(j *job, ref string) {
	defer j.finish()
	start := time.Now()

	named, err := name.ParseReference(ref)
	if err != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("invalid reference %q: %v", ref, err)})
		return
	}
	j.log("resolving %s", named.Name())

	desc, err := remote.Get(named, s.remoteAuth)
	if err != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("fetch failed: %v", err)})
		return
	}

	seeds, platforms, isIndex, err := gatherLayers(s, desc)
	if err != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("manifest read failed: %v", err)})
		return
	}

	sink := &packSink{
		j:        j,
		start:    start,
		byDigest: map[string]*layerProg{},
		prevTime: start,
	}
	allCached := true
	for i, sd := range seeds {
		lp := &layerProg{Index: i, Digest: sd.digest, Size: sd.size, Cached: sd.cached}
		if sd.cached {
			lp.Received = sd.size
			lp.Done = true
			sink.received += sd.size
		} else {
			allCached = false
		}
		sink.total += sd.size
		sink.layers = append(sink.layers, lp)
		sink.byDigest[sd.digest] = lp
	}

	j.emit("job", map[string]any{
		"id":         j.id,
		"kind":       "pack",
		"ref":        ref,
		"name":       named.Name(),
		"mediaType":  string(desc.MediaType),
		"isIndex":    isIndex,
		"platforms":  platforms,
		"totalBytes": sink.total,
		"cached":     allCached,
		"layers":     sink.layers,
	})

	lyt, err := openStore(s.storeDir)
	if err != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("open store failed: %v", err)})
		return
	}

	if allCached {
		j.log("all %d layers already in store", len(seeds))
	} else {
		j.log("pulling %d layers (%s)", len(seeds), humanBytes(sink.total))
	}

	// Resolve the writeable object first so resolution errors don't race the
	// progress ticker.
	var writeBlobs func() error
	var topDescriptor func() (*v1.Descriptor, error)
	if isIndex {
		idx, err := desc.ImageIndex()
		if err != nil {
			j.emit("error", map[string]string{"msg": err.Error()})
			return
		}
		pidx := progressIndex{idx: idx, sink: sink}
		writeBlobs = func() error { return lyt.WriteIndex(pidx) }
		topDescriptor = func() (*v1.Descriptor, error) { return partial.Descriptor(idx) }
	} else {
		img, err := desc.Image()
		if err != nil {
			j.emit("error", map[string]string{"msg": err.Error()})
			return
		}
		pimg := progressImage{Image: img, sink: sink}
		writeBlobs = func() error { return lyt.WriteImage(pimg) }
		topDescriptor = func() (*v1.Descriptor, error) { return partial.Descriptor(img) }
	}

	// Write blobs (content-addressed, concurrent-safe). Bytes flow through the
	// wrapped readers into sink.add; the ticker emits throttled snapshots.
	stop := make(chan struct{})
	tickerDone := runTicker(sink.emit, stop)
	writeErr := writeBlobs()
	close(stop)
	<-tickerDone

	if writeErr != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("write failed: %v", writeErr)})
		return
	}
	topDesc, err := topDescriptor()
	if err != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("descriptor failed: %v", err)})
		return
	}

	// Update index.json: replace any existing entry for this ref, then append.
	if topDesc.Annotations == nil {
		topDesc.Annotations = map[string]string{}
	}
	topDesc.Annotations[refAnnotationKey] = ref

	storeMu.Lock()
	_ = lyt.RemoveDescriptors(func(d v1.Descriptor) bool {
		return d.Annotations[refAnnotationKey] == ref
	})
	appendErr := lyt.AppendDescriptor(*topDesc)
	storeMu.Unlock()
	if appendErr != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("index update failed: %v", appendErr)})
		return
	}

	sink.emit(true)
	j.emit("done", map[string]any{
		"ok":         true,
		"ref":        ref,
		"digest":     topDesc.Digest.String(),
		"totalBytes": sink.total,
		"durationMs": time.Since(start).Milliseconds(),
		"cached":     allCached,
	})
	j.log("saved %s", ref)
}

// ---- UNPACK (serve) ------------------------------------------------------

func (s *server) ensureRegistry() (string, error) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	if s.regStarted {
		return s.regAddr, nil
	}
	bind := s.regBindAddr
	if bind == "" {
		bind = "127.0.0.1:5000"
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return "", err
	}
	blobsDir := filepath.Join(s.storeDir, "blobs")
	handler := &layoutBlobHandler{blobsDir: blobsDir}
	reg := registry.New(
		registry.WithReferrersSupport(false),
		registry.WithBlobHandler(handler),
	)
	srv := &http.Server{Handler: reg}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("registry server error: %v", err)
		}
	}()
	s.regStarted = true
	s.regAddr = ln.Addr().String()
	return s.regAddr, nil
}

type unpackImage struct {
	Index   int    `json:"i"`
	Ref     string `json:"ref"`
	IsIndex bool   `json:"isIndex"`
	Total   int64  `json:"total"`
	desc    v1.Descriptor
}

type unpackSink struct {
	j      *job
	total  int64
	images []*struct {
		Received int64
		Total    int64
		Done     bool
	}

	mu       sync.Mutex
	received int64
	served   int
}

func (u *unpackSink) update(i int, complete, total int64) {
	u.mu.Lock()
	im := u.images[i]
	if total > 0 {
		im.Total = total
	}
	if complete > im.Received {
		u.received += complete - im.Received
		im.Received = complete
	}
	u.mu.Unlock()
}

// emit matches runTicker's func(done bool) shape; the done flag is unused here
// because per-image completion is signalled separately via the "served" event.
func (u *unpackSink) emit(bool) {
	u.mu.Lock()
	ls := make([]map[string]any, len(u.images))
	for i, im := range u.images {
		ls[i] = map[string]any{"i": i, "received": im.Received, "total": im.Total, "done": im.Done}
	}
	recv, total, served := u.received, u.total, u.served
	u.mu.Unlock()
	u.j.emit("progress", map[string]any{
		"received":    recv,
		"total":       total,
		"servedCount": served,
		"images":      ls,
	})
}

func (u *unpackSink) markServed(i int, ref string) {
	u.mu.Lock()
	im := u.images[i]
	if im.Total > im.Received {
		u.received += im.Total - im.Received
		im.Received = im.Total
	}
	im.Done = true
	u.served++
	u.mu.Unlock()
	u.j.emit("served", map[string]any{"i": i, "ref": ref})
}

func (s *server) runUnpack(j *job) {
	defer j.finish()
	start := time.Now()

	addr, err := s.ensureRegistry()
	if err != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("registry start failed: %v", err)})
		return
	}
	j.log("registry listening on %s", addr)

	idx, err := layout.ImageIndexFromPath(s.storeDir)
	if err != nil {
		j.emit("error", map[string]string{"msg": fmt.Sprintf("load store failed: %v", err)})
		return
	}
	im, err := idx.IndexManifest()
	if err != nil {
		j.emit("error", map[string]string{"msg": err.Error()})
		return
	}

	var imgs []*unpackImage
	for _, desc := range im.Manifests {
		ref := desc.Annotations[refAnnotationKey]
		if ref == "" {
			continue
		}
		ui := &unpackImage{Ref: ref, IsIndex: desc.MediaType.IsIndex(), desc: desc}
		// total = sum of layer sizes (approx; blobs already on disk push fast)
		if desc.MediaType.IsIndex() {
			if ci, err := idx.ImageIndex(desc.Digest); err == nil {
				if cm, err := ci.IndexManifest(); err == nil {
					for _, m := range cm.Manifests {
						if cimg, err := ci.Image(m.Digest); err == nil {
							sz, _ := imageBytes(cimg)
							ui.Total += sz
						}
					}
				}
			}
		} else if img, err := idx.Image(desc.Digest); err == nil {
			ui.Total, _ = imageBytes(img)
		}
		imgs = append(imgs, ui)
	}

	sink := &unpackSink{j: j}
	for i, ui := range imgs {
		ui.Index = i
		sink.total += ui.Total
		sink.images = append(sink.images, &struct {
			Received int64
			Total    int64
			Done     bool
		}{Total: ui.Total})
	}

	j.emit("job", map[string]any{
		"id":         j.id,
		"kind":       "unpack",
		"addr":       addr,
		"count":      len(imgs),
		"totalBytes": sink.total,
		"images":     imgs,
	})

	if len(imgs) == 0 {
		j.log("store is empty — nothing to serve")
		j.emit("done", map[string]any{"ok": true, "addr": addr, "count": 0, "durationMs": time.Since(start).Milliseconds()})
		return
	}

	j.log("loading %d images into registry", len(imgs))

	stop := make(chan struct{})
	tickerDone := runTicker(sink.emit, stop)

	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for _, ui := range imgs {
		wg.Add(1)
		sem <- struct{}{}
		go func(ui *unpackImage) {
			defer wg.Done()
			defer func() { <-sem }()

			dest := fmt.Sprintf("%s/%s", addr, ui.Ref)
			destRef, err := name.ParseReference(dest, name.Insecure)
			if err != nil {
				j.log("skip %s: %v", ui.Ref, err)
				return
			}

			updates := make(chan v1.Update, 64)
			go func() {
				for up := range updates {
					if up.Error != nil {
						continue
					}
					sink.update(ui.Index, up.Complete, up.Total)
				}
			}()

			if ui.IsIndex {
				ci, err := idx.ImageIndex(ui.desc.Digest)
				if err != nil {
					close(updates)
					j.log("load index %s failed: %v", ui.Ref, err)
					return
				}
				err = remote.WriteIndex(destRef, ci, remote.WithProgress(updates))
				if err != nil {
					j.log("push %s failed: %v", ui.Ref, err)
					return
				}
			} else {
				cimg, err := idx.Image(ui.desc.Digest)
				if err != nil {
					close(updates)
					j.log("load %s failed: %v", ui.Ref, err)
					return
				}
				err = remote.Write(destRef, cimg, remote.WithProgress(updates))
				if err != nil {
					j.log("push %s failed: %v", ui.Ref, err)
					return
				}
			}
			sink.markServed(ui.Index, ui.Ref)
		}(ui)
	}
	wg.Wait()

	close(stop)
	<-tickerDone
	sink.emit(true)

	j.emit("done", map[string]any{
		"ok":         true,
		"addr":       addr,
		"count":      len(imgs),
		"durationMs": time.Since(start).Milliseconds(),
	})
	j.log("registry ready on %s", addr)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// chartInfo describes a Helm chart packed into the store under ./store/helm/.
type chartInfo struct {
	Repo        string `json:"repo"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Size        int64  `json:"size"`
	File        string `json:"file"`
}

// enumerateCharts walks <storeDir>/helm/<repo>/*.tgz and reads each chart's
// Chart.yaml metadata. Mirrors how `gappy serve` discovers Helm repos.
func enumerateCharts(storeDir string) ([]chartInfo, error) {
	helmBase := filepath.Join(storeDir, "helm")
	repos, err := os.ReadDir(helmBase)
	if err != nil {
		if os.IsNotExist(err) {
			return []chartInfo{}, nil
		}
		return nil, err
	}
	out := []chartInfo{}
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		tgzs, _ := filepath.Glob(filepath.Join(helmBase, repo.Name(), "*.tgz"))
		for _, f := range tgzs {
			ci := chartInfo{Repo: repo.Name(), File: filepath.Base(f)}
			if fi, err := os.Stat(f); err == nil {
				ci.Size = fi.Size()
			}
			if meta, err := readChartYamlFromTgz(f); err == nil && meta != nil {
				ci.Name, ci.Version, ci.Description = meta.Name, meta.Version, meta.Description
			} else {
				ci.Name = strings.TrimSuffix(ci.File, ".tgz")
			}
			out = append(out, ci)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *server) handleImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := enumerateStore(s.storeDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	charts, err := enumerateCharts(s.storeDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var total, chartBytes int64
	for _, im := range imgs {
		total += im.Size
	}
	for _, c := range charts {
		chartBytes += c.Size
	}
	s.regMu.Lock()
	regRunning, regAddr := s.regStarted, s.regAddr
	s.regMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"storeDir":        s.storeDir,
		"count":           len(imgs),
		"totalBytes":      total,
		"chartCount":      len(charts),
		"chartBytes":      chartBytes,
		"charts":          charts,
		"registryRunning": regRunning,
		"registryAddr":    regAddr,
		"acceptsPush":     true,
		"version":         version,
		"images":          imgs,
	})
}

func (s *server) handlePack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Ref string `json:"ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	body.Ref = strings.TrimSpace(body.Ref)
	if body.Ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ref is required"})
		return
	}
	if _, err := name.ParseReference(body.Ref); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid reference: %v", err)})
		return
	}
	j := s.hub.create("pack")
	go s.runPack(j, body.Ref)
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": j.id})
}

func (s *server) handleUnpack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	j := s.hub.create("unpack")
	go s.runUnpack(j)
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": j.id})
}

func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job")
	j := s.hub.get(id)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	// Wake the cond wait when the client disconnects.
	go func() {
		<-ctx.Done()
		j.mu.Lock()
		j.cond.Broadcast()
		j.mu.Unlock()
	}()

	cursor := 0
	for {
		j.mu.Lock()
		for cursor >= len(j.frames) && !j.done && ctx.Err() == nil {
			j.cond.Wait()
		}
		pending := append([]frame(nil), j.frames[cursor:]...)
		cursor = len(j.frames)
		done := j.done
		j.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		for _, f := range pending {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data)
		}
		flusher.Flush()
		if done {
			return
		}
	}
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

func (s *server) routes() http.Handler {
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/images", s.handleImages)
	mux.HandleFunc("/api/pack", s.handlePack)
	mux.HandleFunc("/api/unpack", s.handleUnpack)
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return mux
}

func cmdWeb(storeDir, addr string) {
	srv := newServer(storeDir)
	log.Printf("gappy web UI → http://%s  (store: %s)", addr, storeDir)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}
