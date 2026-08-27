package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

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
// baseline, which may be an http(s) URL, a .zip/.tar/.tar.gz archive, an OCI
// store or directory tree, or a single manifest file.
func collectBaselineKeys(baselinePath string) (map[string]bool, error) {
	if isHTTPURL(baselinePath) {
		return collectKeysFromURL(baselinePath)
	}

	fi, err := os.Stat(baselinePath)
	if err != nil {
		return nil, fmt.Errorf("stat baseline %s: %w", baselinePath, err)
	}

	switch {
	case fi.IsDir():
		return collectKeysFromDir(baselinePath)
	case strings.HasSuffix(baselinePath, ".zip"):
		return collectKeysFromZipFile(baselinePath)
	case isTarName(baselinePath):
		return collectKeysFromTarFile(baselinePath)
	default:
		return collectKeysFromFile(baselinePath)
	}
}

// isTarName reports whether path names a tar stream, gzipped or not.
func isTarName(path string) bool {
	return strings.HasSuffix(path, ".tar") ||
		strings.HasSuffix(path, ".tar.gz") ||
		strings.HasSuffix(path, ".tgz")
}

// isGzipName reports whether a tar stream named path is gzipped. A bundle's
// .tgz is a compressed tar; a chart's .tgz never reaches here, because
// isHelmArchive claims it first.
func isGzipName(path string) bool {
	return strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz")
}

// --- archive walking -------------------------------------------------------

// archiveEntry is one member of a zip or tar, reduced to what key collection
// needs: the path it was stored under, and a way to read it that the caller
// may decline. Zip and tar disagree about almost everything else — random
// access versus a single forward pass, decompression on open versus on the
// stream — but the rule for which members carry keys is the same, so it lives
// once in collectFromArchive.
type archiveEntry struct {
	name string
	open func() (io.ReadCloser, error)
}

// maxEntryBytes caps how much of a single archive member is read into memory.
// A manifest is kilobytes; anything at this size is a wrong guess about the
// file, or a hostile archive, and either way is not worth the allocation.
const maxEntryBytes = 32 << 20

// collectFromArchive keys every member of an archive that carries refs. label
// names the archive in warnings. next yields entries in order and returns
// io.EOF when done; a nil entry is skipped.
func collectFromArchive(label string, next func() (*archiveEntry, error)) (map[string]bool, error) {
	keys := make(map[string]bool)

	for {
		e, err := next()
		if err == io.EOF {
			return keys, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", label, err)
		}
		if e == nil {
			continue
		}

		base := path.Base(e.name)
		switch {
		case base == "index.json":
			data, err := readEntry(e)
			if err != nil {
				log.Printf("warning: baseline %s!%s: %v", label, e.name, err)
				continue
			}
			var idx ociIndexJSON
			if err := json.Unmarshal(data, &idx); err != nil {
				// Not every index.json in a bundle is an OCI index.
				continue
			}
			collectFromOCIIndex(idx, keys)

		case isHelmArchive(e.name):
			collectFromChartArchive(e.name, keys)

		case isPlainManifestName(base) || isYAMLName(base):
			data, err := readEntry(e)
			if err != nil {
				log.Printf("warning: baseline %s!%s: %v", label, e.name, err)
				continue
			}
			collectFromContent(data, keys, isPlainManifestName(base))
		}
	}
}

func readEntry(e *archiveEntry) ([]byte, error) {
	rc, err := e.open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, maxEntryBytes))
	if err != nil {
		return nil, err
	}
	if len(data) == maxEntryBytes {
		return nil, fmt.Errorf("entry exceeds %s", humanBytes(maxEntryBytes))
	}
	return data, nil
}

func collectKeysFromZipFile(zipPath string) (map[string]bool, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer r.Close()
	return collectFromZipReader(zipPath, &r.Reader)
}

func collectFromZipReader(label string, r *zip.Reader) (map[string]bool, error) {
	i := 0
	return collectFromArchive(label, func() (*archiveEntry, error) {
		if i >= len(r.File) {
			return nil, io.EOF
		}
		f := r.File[i]
		i++
		if f.FileInfo().IsDir() {
			return nil, nil
		}
		return &archiveEntry{name: f.Name, open: func() (io.ReadCloser, error) { return f.Open() }}, nil
	})
}

func collectKeysFromTarFile(tarPath string) (map[string]bool, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", tarPath, err)
	}
	defer f.Close()
	return collectFromTarStream(tarPath, f, isGzipName(tarPath))
}

// collectFromTarStream reads a tar in a single forward pass. Unlike a zip it
// has no central directory, so every byte crosses the wire even though only a
// few members carry keys — which is why a remote .zip baseline is much cheaper
// than a remote .tar.gz one.
func collectFromTarStream(label string, r io.Reader, gzipped bool) (map[string]bool, error) {
	if gzipped {
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("read gzip %s: %w", label, err)
		}
		defer gr.Close()
		r = gr
	}

	tr := tar.NewReader(r)
	return collectFromArchive(label, func() (*archiveEntry, error) {
		hdr, err := tr.Next()
		if err != nil {
			return nil, err // io.EOF ends the walk
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, nil
		}
		// The entry is only readable until the next call to tr.Next, which is
		// exactly the window collectFromArchive uses it in.
		return &archiveEntry{name: hdr.Name, open: func() (io.ReadCloser, error) {
			return io.NopCloser(tr), nil
		}}, nil
	})
}

// --- remote baselines ------------------------------------------------------

// A baseline often lives in Artifactory rather than on disk. A zip there is
// read over ranged GETs: archive/zip needs only the central directory at the
// tail plus the few members that carry refs, so a 20GB bundle costs a few MB
// to diff against. A tar has no central directory and must be streamed whole,
// so prefer a .zip baseline URL when there's a choice.

// errRangeUnsupported means the server answered a ranged GET with the entire
// body. Reading that as the requested range would hand archive/zip the head of
// the file under every offset, so the ranged reader refuses and the caller
// downloads instead.
var errRangeUnsupported = errors.New("server does not support range requests")

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// credsFor resolves basic-auth credentials for u: the URL's own userinfo
// first, then GAPPY_USER/GAPPY_PASS, then RSART_LOCAL_USER/RSART_LOCAL_AUTH.
func credsFor(u *url.URL) (user, pass string) {
	if u.User != nil {
		pass, _ := u.User.Password()
		return u.User.Username(), pass
	}
	if user, pass := os.Getenv("GAPPY_USER"), os.Getenv("GAPPY_PASS"); user != "" || pass != "" {
		return user, pass
	}
	return os.Getenv("RSART_LOCAL_USER"), os.Getenv("RSART_LOCAL_AUTH")
}

// setAuth attaches credentials when there are any. Either half may be empty:
// an Artifactory API key or identity token is commonly sent as the password
// with no username, and requiring both would silently drop it.
func setAuth(req *http.Request, user, pass string) {
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
}

// redactURL renders u for a log line or an error. Credentials reach gappy in
// the URL itself often enough that echoing one back verbatim would leak it
// into a build log, so userinfo is stripped and any query — where Artifactory
// carries a token — is elided.
func redactURL(u *url.URL) string {
	clean := *u
	clean.User = nil
	if clean.RawQuery != "" {
		clean.RawQuery = ""
		return clean.String() + "?..."
	}
	return clean.String()
}

// baselineClient builds a client for baseline fetches. overall bounds the whole
// exchange and so has to allow for the size of the body; responseHeaderTimeout
// is what actually catches a dead or hanging server promptly.
func baselineClient(overall time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: tr, Timeout: overall}
}

func newBaselineRequest(method string, u *url.URL, user, pass string) (*http.Request, error) {
	req, err := http.NewRequest(method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	setAuth(req, user, pass)
	return req, nil
}

func collectKeysFromURL(rawURL string) (map[string]bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse baseline URL: %w", err)
	}
	user, pass := credsFor(u)

	if isTarName(u.Path) {
		return collectKeysFromRemoteTar(u, user, pass)
	}
	return collectKeysFromRemoteZip(u, user, pass)
}

func collectKeysFromRemoteTar(u *url.URL, user, pass string) (map[string]bool, error) {
	label := redactURL(u)

	// A tar is read start to finish, so the timeout has to cover the whole
	// bundle rather than one small range.
	req, err := newBaselineRequest(http.MethodGet, u, user, pass)
	if err != nil {
		return nil, err
	}
	resp, err := baselineClient(30 * time.Minute).Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", label, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %s", label, resp.Status)
	}

	log.Printf("streaming baseline %s (a tar has no index, so all of it is read)", label)
	return collectFromTarStream(label, resp.Body, isGzipName(u.Path))
}

func collectKeysFromRemoteZip(u *url.URL, user, pass string) (map[string]bool, error) {
	label := redactURL(u)
	client := baselineClient(5 * time.Minute)

	size, ranges, err := probeRemote(client, u, user, pass)
	if err != nil {
		return nil, err
	}

	if ranges && size > 0 {
		keys, err := collectFromRangedZip(client, u, user, pass, label, size)
		if err == nil {
			return keys, nil
		}
		if !errors.Is(err, errRangeUnsupported) {
			return nil, err
		}
		// The probe said ranges were available and the server then ignored
		// one. Fall through to the download rather than fail.
	}

	log.Printf("baseline %s: no usable range support, downloading in full", label)
	return collectKeysFromDownloadedZip(client, u, user, pass, label)
}

func collectFromRangedZip(client *http.Client, u *url.URL, user, pass, label string, size int64) (map[string]bool, error) {
	r := &httpReaderAt{client: client, url: u, user: user, pass: pass, size: size}

	zr, err := zip.NewReader(r, size)
	if err != nil {
		if errors.Is(err, errRangeUnsupported) {
			return nil, err
		}
		return nil, fmt.Errorf("read remote zip %s: %w", label, err)
	}
	return collectFromZipReader(label, zr)
}

func collectKeysFromDownloadedZip(client *http.Client, u *url.URL, user, pass, label string) (map[string]bool, error) {
	req, err := newBaselineRequest(http.MethodGet, u, user, pass)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", label, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %s", label, resp.Status)
	}

	// archive/zip needs a ReaderAt, so this has to land somewhere seekable.
	// A temp file rather than memory, because the whole point of the remote
	// baseline is that these archives are large.
	tmp, err := os.CreateTemp("", "gappy-baseline-*.zip")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return nil, fmt.Errorf("download %s: %w", label, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	keys, err := collectKeysFromZipFile(tmp.Name())
	if err != nil {
		// Don't name the temp file at the user, it's already gone.
		return nil, fmt.Errorf("read downloaded baseline %s: %w", label, err)
	}
	return keys, nil
}

// probeRemote reports the size of the remote archive and whether the server
// will serve ranges of it.
func probeRemote(client *http.Client, u *url.URL, user, pass string) (size int64, ranges bool, err error) {
	label := redactURL(u)

	req, err := newBaselineRequest(http.MethodHead, u, user, pass)
	if err != nil {
		return 0, false, err
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.ContentLength, strings.Contains(resp.Header.Get("Accept-Ranges"), "bytes"), nil
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return 0, false, fmt.Errorf("head %s: %s (set GAPPY_USER and GAPPY_PASS)", label, resp.Status)
		}
		// Some servers refuse HEAD outright; a one-byte range answers both
		// questions at once.
	}

	req, err = newBaselineRequest(http.MethodGet, u, user, pass)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err = client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("get %s: %w", label, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		return sizeFromContentRange(resp.Header.Get("Content-Range")), true, nil
	case http.StatusOK:
		// Ranges ignored, so the size here is the whole archive.
		return resp.ContentLength, false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, false, fmt.Errorf("get %s: %s (set GAPPY_USER and GAPPY_PASS)", label, resp.Status)
	default:
		return 0, false, fmt.Errorf("get %s: %s", label, resp.Status)
	}
}

// sizeFromContentRange pulls the total length out of "bytes 0-0/12345",
// returning 0 for the "*" that means the server won't say.
func sizeFromContentRange(v string) int64 {
	_, total, ok := strings.Cut(v, "/")
	if !ok {
		return 0
	}
	var n int64
	if _, err := fmt.Sscanf(total, "%d", &n); err != nil {
		return 0
	}
	return n
}

// httpReaderAt adapts ranged GETs to the io.ReaderAt that archive/zip wants.
type httpReaderAt struct {
	client *http.Client
	url    *url.URL
	user   string
	pass   string
	size   int64
}

func (h *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || off >= h.size {
		return 0, io.EOF
	}

	// Ranges are inclusive on both ends, and asking past the last byte makes
	// some servers answer 416 instead of a short read.
	end := off + int64(len(p)) - 1
	if end >= h.size {
		end = h.size - 1
	}
	want := int(end - off + 1)

	req, err := newBaselineRequest(http.MethodGet, h.url, h.user, h.pass)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		// The body is the whole archive from byte 0. Copying it into p would
		// satisfy the read with the wrong bytes and leave archive/zip parsing
		// the head of the file as though it were the central directory.
		return 0, errRangeUnsupported
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range %d-%d of %s: %s", off, end, redactURL(h.url), resp.Status)
	}

	n, err := io.ReadFull(resp.Body, p[:want])
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	if err == nil && n < len(p) {
		// Clamped against the end of the archive: a short read, per the
		// io.ReaderAt contract, needs an error saying why.
		err = io.EOF
	}
	return n, err
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
