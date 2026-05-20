# gappy

Simple but fast airgap tool. 

### Install
go will download source + build a static binary for your platform

```bash
go install github.com/briansterle/gappy@latest
```

### API

```bash

# download all images to local ./store dir in oci layout format
gappy pack images.txt

# downloads images using 8 worker threads (default is # cpus)
gappy -j 8 pack images.txt

# same thing using a hauler store manifest
gappy pack manifest.yaml

# serve the registry at ./store on localhost:5000
gappy serve 
```
