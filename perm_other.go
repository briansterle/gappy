//go:build !unix

package main

import "os"

// Windows has no umask and no uid — permission repair is a no-op there, but the
// rest of gappy still builds and runs.

func setUmask(int) int { return 0 }

func fileOwner(os.FileInfo) (uid string, name string) { return "", "" }
