package main

import (
	"flag"
	"fmt"
	"log"
	"runtime"
	"runtime/debug"
)

// --- build info ------------------------------------------------------------

// Stamped by build.sh via -ldflags; whatever the linker leaves at its default
// is recovered from the binary's own build info.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// dateFromCommit records that date is a commit timestamp rather than a build
// time, so `gappy version` doesn't report one as the other.
var dateFromCommit bool

// buildStamp is the version triple, as a value so the fallback logic can be
// exercised without a real binary behind it.
type buildStamp struct {
	version, commit, date string
	dateFromCommit        bool
}

// withBuildInfo fills in whatever -ldflags didn't stamp. `go install
// module@version` records the module version, and a build from a git checkout
// records the revision and its commit time, so an unstamped binary — the one
// `go install github.com/briansterle/gappy@latest` produces — still reports
// something accurate.
func (b buildStamp) withBuildInfo(bi *debug.BuildInfo) buildStamp {
	if b.version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		b.version = bi.Main.Version
	}

	var revision string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		case "vcs.time":
			if b.date == "unknown" && s.Value != "" {
				b.date, b.dateFromCommit = s.Value, true
			}
		}
	}

	if b.commit == "none" && revision != "" {
		if len(revision) > 7 {
			revision = revision[:7]
		}
		b.commit = revision
		if dirty {
			b.commit += "-dirty"
		}
	}
	return b
}

func resolveBuildInfo() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	s := buildStamp{version: version, commit: commit, date: date}.withBuildInfo(bi)
	version, commit, date, dateFromCommit = s.version, s.commit, s.date, s.dateFromCommit
}

func cmdVersion() {
	fmt.Printf("gappy %s\n", version)
	fmt.Printf("  commit:  %s\n", commit)
	built := date
	if dateFromCommit {
		built += "  (commit time)"
	}
	fmt.Printf("  built:   %s\n", built)
	fmt.Printf("  go:      %s\n", runtime.Version())
	fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

// --- flags and dispatch ----------------------------------------------------

var jobs int

var showVersion bool

func init() {
	flag.IntVar(&jobs, "j", max(1, runtime.NumCPU()-1), "parallel jobs")
	flag.BoolVar(&showVersion, "version", false, "print version info and exit")
	flag.BoolVar(&showVersion, "v", false, "print version info and exit")
}

func main() {
	flag.Parse()
	resolveBuildInfo()
	if showVersion {
		cmdVersion()
		return
	}
	args := flag.Args()
	if len(args) < 1 {
		log.Fatal("usage:\n  gappy [-j N] pack <images.txt|manifest.yaml>\n  gappy [-j N] pack-charts <found-charts.txt|manifest.yaml>\n  gappy diff <baseline|zip|tar|url|dir> <current-file> [out-file]\n  gappy serve [store-path]\n  gappy discover [dir]\n  gappy rmi <ref|digest> [store-path]\n  gappy verify [store-path]\n  gappy split <dvd|dvd9|bd25|bd50|bd100|SIZE> [store] [out]\n  gappy join <out-dir> <disc-001> [disc-002 ...]\n  gappy merge [-n] <base-store> <store> [store ...]\n  gappy fix-perms [-n] [store-path]\n  gappy version")
	}

	switch args[0] {
	case "diff":
		if len(args) < 3 {
			log.Fatal("usage: gappy diff <baseline|zip|tar|url|dir> <current-file> [out-file]")
		}
		outFile := ""
		if len(args) >= 4 {
			outFile = args[3]
		}
		cmdDiff(args[1], args[2], outFile)
	case "pack":
		if len(args) < 2 {
			log.Fatal("usage: gappy pack <images.txt|manifest.yaml>")
		}
		cmdPack(args[1])
	case "pack-charts":
		if len(args) < 2 {
			log.Fatal("usage: gappy pack-charts <found-charts.txt|manifest.yaml>")
		}
		cmdPackCharts(args[1])
	case "serve":
		storePath := "./store"
		if len(args) >= 2 {
			storePath = args[1]
		}
		cmdServe(storePath)
	case "discover":
		root := "."
		if len(args) >= 2 {
			root = args[1]
		}
		cmdDiscover(root)
	case "rmi":
		if len(args) < 2 {
			log.Fatal("usage: gappy rmi <ref|digest> [store-path]")
		}
		storeDir := "./store"
		if len(args) >= 3 {
			storeDir = args[2]
		}
		cmdRmi(storeDir, args[1])
	case "verify":
		storeDir := "./store"
		if len(args) >= 2 {
			storeDir = args[1]
		}
		cmdVerify(storeDir)
	case "split":
		if len(args) < 2 {
			log.Fatal("usage: gappy split <dvd|dvd9|bd25|bd50|bd100|SIZE> [store-path] [out-dir]")
		}
		storeDir := "./store"
		outDir := "."
		if len(args) >= 3 {
			storeDir = args[2]
		}
		if len(args) >= 4 {
			outDir = args[3]
		}
		cmdSplit(args[1], storeDir, outDir)
	case "join":
		if len(args) < 3 {
			log.Fatal("usage: gappy join <out-dir> <disc-001> [disc-002 ...]")
		}
		cmdJoin(args[1], args[2:])
	case "fix-perms":
		rest, dryRun := extractDryRun(args[1:])
		storeDir := "./store"
		if len(rest) >= 1 {
			storeDir = rest[0]
		}
		cmdFixPerms(storeDir, dryRun)
	case "merge":
		rest, dryRun := extractDryRun(args[1:])
		if len(rest) < 2 {
			log.Fatal("usage: gappy merge [-n] <base-store> <store> [store ...]")
		}
		cmdMerge(rest[0], rest[1:], dryRun)
	case "version":
		cmdVersion()
	default:
		log.Fatalf("unknown command %q — use pack, pack-charts, diff, serve, discover, rmi, verify, split, join, merge, fix-perms, or version", args[0])
	}
}
