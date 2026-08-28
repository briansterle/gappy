# gappy

Packs container images and Helm charts into a portable OCI store, then serves
them in an air-gapped environment. A faster, lighter alternative to hauler.

## Why gappy

| Capability | gappy | hauler | zarf | skopeo / crane |
|---|---|---|---|---|
| **Binary size** | ~10 MB | ~100 MB | ~150 MB+ | ~20–40 MB |
| **Dependencies** | `go-containerregistry` only | Helm SDK, k8s client-go, ... | Custom runtime & CLI | Distribution tools |
| **Parallel downloads** | Yes (`-j N`, default: CPU count - 1) | No (single-threaded) | Yes | Scripting required |
| **Unified server (OCI + Helm HTTP)** | Yes (single port `:5000`) | Separate listeners | Multi-port | None (client only) |
| **Delta diff against baseline** | Yes (`gappy diff`) | No | Differential pkg | No |
| **In-place store merge** | Yes (`gappy merge`) | No | No | No |
| **Physical media split / join** | Yes (`gappy split` / `join`) | No | Manual | No |
| **Push-through airgap registry** | Yes (`docker push`) | Limited | Cluster registry | None |
| **Skip cached artifacts** | Yes (digest check) | Yes | Yes | Yes |
| **Hauler manifest support** | Yes (`kind: Images`, `kind: Charts`) | Yes | No | No |

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
gappy [-j N] pack <images.txt|manifest.yaml>               pack container images
gappy [-j N] pack-charts <charts.txt|manifest.yaml>        pack Helm charts
gappy serve [store-path]                                   serve images + charts
gappy discover [dir]                                       find image and chart refs
gappy rmi <ref|digest> [store-path]                        remove an image and gc orphaned blobs
gappy verify [store-path]                                  check every blob against its digest
gappy split <dvd|dvd9|bd25|bd50|bd100|SIZE> [store] [out]  split store into disc volumes
gappy join <out-dir> <disc-001> [disc-002 ...]             merge disc volumes into a store
gappy merge [-n] <base-store> <store> [store ...]          fold stores into base-store
gappy diff <baseline|zip|tar|url|dir> <manifest> [out]     filter manifest down to missing items
gappy fix-perms [-n] [store-path]                          repair store permissions
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

Image registries, Helm repos, and `gappy diff` baseline URLs share the same
credentials:

```bash
export GAPPY_USER=myuser
export GAPPY_PASS=mypassword
```

Either may be empty on its own — an Artifactory identity token is sent as the
password with no username.

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

### 6. Merge an incremental store

Rather than re-carrying the whole store across the gap every time, pack only
what changed into a second store and fold it into the big one on the far side:

```bash
gappy merge ./store ./incremental        # ./store is the side that grows
gappy merge -n ./store ./incremental     # dry run — print what would change
gappy merge ./store ./inc-a ./inc-b      # several increments in one pass
```

It's a left join: only the incremental stores are read, so folding a 2 GB delta
into a 500 GB store moves 2 GB, not 502 GB. Blobs are content-addressed, so
anything the base already holds is skipped by digest; the rest are hardlinked
when both stores are on one filesystem and copied when they aren't. Chart
archives merge the same way, keyed on `repo/name-version.tgz`.

An incremental image whose tag already exists in the base *with a different
digest* replaces it — the same thing `pack` does when a tag moves:

```
merging 1 store(s) into ./store

  ./incremental
    ~ redis:alpine       sha256:03a36f10e292 → sha256:7b011c741198
    + newapp:v1          sha256:814e9c46ddc4
    copying 10 blob(s), 6.0 MiB
    + helm/my-repo/brand-new-2.0.0.tgz

  images  +1 added  ~1 updated  =0 unchanged
  blobs   +10 (6.0 MiB)  0 already present
  charts  +1 (7 B)  1 already present

merged → ./store  (4 image(s) total)
```

The replaced image's blobs stay on disk as orphans; `gappy verify` counts them.

## Delta diffs

Once a bundle has shipped, the next one usually only needs what changed.
`gappy diff` filters a discovered manifest down to the entries a baseline
doesn't already carry:

```bash
gappy diff baseline.zip found-images.txt found-images.diff.txt
gappy diff baseline.zip found-charts.txt found-charts.diff.txt
```

Pack the filtered list as usual, then fold the result into the far-side store
with [`gappy merge`](#6-merge-an-incremental-store).

A baseline can be a `.zip`, a `.tar`/`.tar.gz`, a store or directory tree, a
single manifest file, or an `http(s)` URL to any of those:

```bash
gappy diff https://artifactory.example.com/repo/baseline-1.4.0.zip \
  found-images.txt found-images.diff.txt
```

Prefer a `.zip` URL when a baseline is published as both: a remote zip is read
with range requests, so only its index and the few members holding refs cross
the wire, while a `.tar.gz` has to be streamed in full. Baseline URLs take the
same `GAPPY_USER` / `GAPPY_PASS` credentials as everything else, and any
credentials in the URL itself are stripped from what gappy logs.

An entry is dropped only on an exact match. A baseline gappy can't read is
reported as covering nothing, with a warning — over-shipping is recoverable,
a silently missing image is not.

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

## Shared stores and permissions

A store on shared storage — an NFS export at `/projects/foobar/store`, say — gets
written by whoever runs `pack`, `merge`, or `docker push` that day, and the mode
of everything they write is decided by *their* umask. One person with `umask 077`
leaves a store nobody else can read, and under root squash nobody can chown their
way out of it.

gappy takes its permission policy from the store directory itself. Set it once:

```bash
chgrp -R foobar-team /projects/foobar/store
chmod 2775 /projects/foobar/store          # group-writable, setgid
```

Every gappy write then matches — `2775` directories, `0664` files — whatever the
writer's umask. Keep the setgid bit: it's what puts new files in the team's group
rather than each writer's primary group. A `0755` store root gives `0755`/`0644`
instead: readable by everyone, writable by one person.

This has to cover writes gappy doesn't make itself — `pack` hands blob creation to
go-containerregistry — so gappy sets the process umask from the policy rather than
only chmod'ing its own files.

Repair a store that's already wrong:

```bash
gappy fix-perms /projects/foobar/store     # -n to preview
```

It chmods everything the running user owns. Files owned by someone else can only
be chmod'ed by them, so those are reported grouped by owner:

```
/projects/foobar/store  (dirs 2775, files 0664 — taken from the store directory)

  fixed     1284 path(s)
  ok        9912 path(s) already correct
  blocked   12 path(s) owned by someone else

    bob (uid 1042)             12 path(s)   e.g. blobs/sha256/9f3a1c4e8b02

  only the owner can chmod their own files. ask each of them to run:
    gappy fix-perms /projects/foobar/store
```

`gappy verify` counts unreadable blobs separately from corrupt ones, so a
permissions problem doesn't read as a bad transfer.

## Skip-if-cached

`pack` and `pack-charts` check the store for an existing blob (by digest)
before pulling, so re-running against an unchanged manifest only downloads
what's new.

## Version

```bash
gappy version    # or: gappy -v
# gappy v1.2.0
#   commit:  f14723f
#   built:   2026-08-27T14:12:04Z
#   go:      go1.26.3
#   os/arch: linux/amd64
```

`build.sh` injects the stamps via `-ldflags`, taking the version from
`git describe`. A binary built without them — anything from
`go install github.com/briansterle/gappy@latest`, or a plain `go build` —
recovers what it can from the build info Go embeds: the module version for an
installed binary, the revision and commit time for a build from a checkout.
