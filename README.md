# gappy

Packs container images and Helm charts into a portable OCI store, then serves
them in an air-gapped environment. A faster, lighter alternative to hauler.

## Why gappy

| | gappy | hauler |
|---|---|---|
| Binary size | ~10 MB | ~100 MB |
| Dependencies | `go-containerregistry` only | helm SDK, k8s client-go, ... |
| Parallel downloads | Yes (`-j N`, default: CPU count) | No (single-threaded) |
| Helm repo serving | Single port (OCI + HTTP) | Separate |
| Skip cached artifacts | Yes (digest check) | Yes |
| Hauler manifest support | Yes | Yes |

`-j 12` on a large manifest is typically 2-5x faster than hauler's sequential
pull. Otherwise gappy does one job: pack on a connected machine, carry the
store across the gap, serve it locally.

## Install

```bash
go install github.com/briansterle/gappy@latest
```

Or build from source:

```bash
bash build.sh
cp gappy ~/bin/
```

Requires Go 1.25+.

## Commands

```
gappy [-j N] pack <images.txt|manifest.yaml>              pack container images
gappy [-j N] pack-charts <charts.txt|manifest.yaml>        pack Helm charts
gappy serve [store-path]                                   serve images + charts
gappy discover [dir]                                       find image and chart refs
gappy rmi <ref|digest> [store-path]                        remove an image and gc orphaned blobs
gappy split <dvd|dvd9|bd25|bd50|bd100|SIZE> [store] [out]  split store into disc volumes
gappy join <out-dir> <disc-001> [disc-002 ...]             merge disc volumes into a store
gappy version                                              print version info
```

`-j N` sets parallel download workers (default: CPU count - 1).

## Workflow

### 1. Discover

Scan a directory tree for image and chart refs:

```bash
gappy discover templates     # finds image refs → found-images.txt
gappy discover charts        # finds chart refs → found-charts.txt
```

### 2. Pack images

Accepts a plain text file (one image per line) or a Hauler image manifest:

```bash
gappy -j 12 pack hauler/hauler-image-manifest.yaml
gappy -j 12 pack found-images.txt
```

Images land in an OCI layout at `./store`.

Hauler manifest format (`kind: Images`):

```yaml
apiVersion: content.hauler.cattle.io/v1
kind: Images
metadata:
  name: my-images
spec:
  images:
    - name: nginx:alpine
    - name: redis:alpine
```

### 3. Pack charts

Accepts a Hauler chart manifest or the `found-charts.txt` produced by `discover`:

```bash
gappy pack-charts hauler/hauler-chart-manifest.yaml
gappy pack-charts found-charts.txt
```

Hauler manifest format (`kind: Charts`):

```yaml
apiVersion: content.hauler.cattle.io/v1
kind: Charts
metadata:
  name: my-charts
spec:
  charts:
    - name: my-chart
      version: 1.2.3
      repoURL: my-helm-repo
    - name: my-oci-chart
      version: 2.0.0
      repoURL: my-helm-repo-oci
```

`repoURL` is a Helm repo alias, resolved via `~/.config/helm/repositories.yaml`
(the same file Helm itself uses). HTTP repos download as `.tgz`; OCI repos go
into the OCI layout.

Image registries and Helm repos share the same credentials:

```bash
export GAPPY_USER=myuser
export GAPPY_PASS=mypassword
```

### 4. Serve

```bash
gappy serve              # uses ./store
gappy serve /path/store  # explicit path
```

One server on `:5000` handles everything:

| Path | Protocol | Content |
|---|---|---|
| `/v2/...` | OCI Distribution | Container images |
| `/{repoName}/index.yaml` | Helm HTTP | Chart index |
| `/{repoName}/{chart}-{version}.tgz` | Helm HTTP | Chart package |

Helm HTTP repos are auto-discovered from subdirectories of `./store/helm/` at
startup. In the air gap:

```bash
helm repo add my-helm-repo http://localhost:5000/my-helm-repo
helm pull my-helm-repo/my-chart --version 1.2.3
```

**Pushing in.** The `/v2/` endpoint takes `docker push`, so you can add images
to a running store from inside the air gap:

```bash
docker tag myapp:v1 localhost:5000/myapp:v1
docker push localhost:5000/myapp:v1
```

Pushed blobs land straight in the OCI layout, so they persist and travel with
the store like any packed image — single-arch and multi-arch both work. The
registry serves plain HTTP on `localhost`, which Docker treats as insecure by
default, so no extra daemon config is needed.

### 5. Split onto physical media

For air gaps crossed by DVD/Blu-ray, split the store into disc-sized volumes:

```bash
gappy split dvd                    # DVD-5, 4.7 GB
gappy split bd25                   # Blu-ray BD-25, 25 GB
gappy split 4.7GB ./store ./discs  # custom size
```

Presets: `dvd`, `dvd9`, `bd25`, `bd50`, `bd100`. Each `disc-NNN/` is a valid
OCI layout on its own — `gappy serve disc-001/` works straight off a disc.
Blobs are de-duplicated both within and across discs.

Reassemble after transport:

```bash
gappy join merged-store /mnt/disc-001 /mnt/disc-002 /mnt/disc-003
```

## Store layout

```
store/
├── blobs/sha256/        OCI content-addressable blobs (images + OCI charts)
├── index.json           OCI layout index
├── oci-layout
└── helm/
    └── my-helm-repo/    HTTP Helm repo charts
        ├── my-chart-1.2.3.tgz
        └── ...
```

## Skip-if-cached

`pack` and `pack-charts` check the store for an existing blob (by digest)
before pulling, so re-running against an unchanged manifest only downloads
what's new.

## Version

```bash
gappy version
# gappy v0.0.5
#   commit:  b64fd24
#   built:   2026-05-29T18:43:00Z
#   go:      go1.26.3
#   os/arch: linux/amd64
```

Build stamps are injected by `build.sh` via `-ldflags`.
