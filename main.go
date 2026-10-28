package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
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

// --- help ------------------------------------------------------------------

const helpText = `Packs container images and Helm charts into a portable OCI store, then
serves or pushes them on the far side of an air gap.

Usage:
  gappy [-j N] <command> [args]

Pack (connected side):
  discover [dir]                               find image and chart refs under dir
  pack <images.txt|manifest.yaml>              pull images into ./store
  pack-charts <charts.txt|manifest.yaml>       pull Helm charts into ./store
  diff <baseline> <manifest> [out]             drop entries a baseline already has

Carry:
  split <dvd|dvd9|bd25|bd50|bd100|SIZE> [store] [out]
                                               split the store into disc volumes
  join <out-dir> <disc-001> [disc-002 ...]     reassemble disc volumes
  merge [-n] <base-store> <store> [store ...]  fold incremental stores into base

Use (air-gapped side):
  serve [store]                                serve images and charts on :5000
  push [-n] [--helm <repo-url>] <prefix> [store]
                                               push images, and charts with
                                               --helm, to a registry

Maintain:
  verify [store]                               check every blob against its digest
  rmi <ref|digest> [store]                     remove an image, gc its blobs
  fix-perms [-n] [store]                       repair store permissions
  version                                      print version info
  help                                         print this help

A store argument defaults to ./store.

Flags:
  -j N   parallel workers (default: CPU count - 1, or 4 for push)
  -n     dry run for merge, push, and fix-perms
  -v     print version info

Environment:
  GAPPY_USER, GAPPY_PASS
         credentials for registries, Helm repos, and baseline URLs.
         Either may be empty, so a token can go in GAPPY_PASS alone.
         With neither set, registries use your Docker credentials.

Examples:
  gappy -j 12 pack found-images.txt
  gappy push artifactory.company.org/dev-docker
  gappy push --helm https://artifactory.company.org/artifactory/api/helm/dev-helm \
      artifactory.company.org/dev-docker

--helm takes https:// for a classic Helm repo or oci:// for a Helm OCI repo.
More: https://github.com/briansterle/gappy
`

func cmdHelp(w io.Writer) {
	fmt.Fprintf(w, "gappy %s (commit %s)\n\n%s", version, commit, helpText)
}

func isHelpArg(a string) bool { return a == "-h" || a == "-help" || a == "--help" }

// --- flags and dispatch ----------------------------------------------------

var jobs int

// pushJobs caps push's default -j. Each image already uploads 4 blobs at
// once, so CPU count on a big box would open hundreds of uploads against one
// registry.
const pushJobs = 4

var showVersion, showHelp bool

func init() {
	flag.IntVar(&jobs, "j", max(1, runtime.NumCPU()-1), "parallel jobs")
	flag.BoolVar(&showVersion, "version", false, "print version info and exit")
	flag.BoolVar(&showVersion, "v", false, "print version info and exit")
	flag.BoolVar(&showHelp, "help", false, "print help and exit")
	flag.BoolVar(&showHelp, "h", false, "print help and exit")
	flag.Usage = func() { cmdHelp(flag.CommandLine.Output()) }
}

// flagSet reports whether the named flag was given on the command line.
func flagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// extractFlagValue pulls "-name value" or "-name=value", with one or two
// dashes, out of a subcommand's args.
func extractFlagValue(args []string, name string) ([]string, string) {
	rest := make([]string, 0, len(args))
	value := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-"+name || a == "--"+name:
			if i+1 == len(args) {
				log.Fatalf("-%s needs a value", name)
			}
			i++
			value = args[i]
		case strings.HasPrefix(a, "-"+name+"=") || strings.HasPrefix(a, "--"+name+"="):
			value = a[strings.Index(a, "=")+1:]
		default:
			rest = append(rest, a)
		}
	}
	return rest, value
}

func main() {
	resolveBuildInfo()
	flag.Parse()
	if showHelp {
		cmdHelp(os.Stdout)
		return
	}
	if showVersion {
		cmdVersion()
		return
	}
	args := flag.Args()
	if len(args) < 1 {
		cmdHelp(os.Stderr)
		os.Exit(2)
	}
	if slices.ContainsFunc(args, isHelpArg) {
		cmdHelp(os.Stdout)
		return
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
	case "push":
		rest, dryRun := extractDryRun(args[1:])
		rest, helmRepo := extractFlagValue(rest, "helm")
		if len(rest) < 1 {
			log.Fatal("usage: gappy push [-n] [--helm <repo-url>] <registry/prefix> [store-path]")
		}
		storeDir := "./store"
		if len(rest) >= 2 {
			storeDir = rest[1]
		}
		if !flagSet("j") {
			jobs = pushJobs
		}
		cmdPush(rest[0], helmRepo, storeDir, dryRun)
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
	case "help":
		cmdHelp(os.Stdout)
	default:
		log.Fatalf("unknown command %q — run gappy help for the list", args[0])
	}
}
