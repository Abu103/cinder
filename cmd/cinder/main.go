// Command cinder is the command-line front end for the store.
//
//	cinder set <key> <value...>
//	cinder get <key>
//	cinder del <key>
//	cinder compact
//
// Every invocation opens the store (which replays the log), does one thing, and
// closes it. That makes the CLI an end-to-end test of recovery on every call.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"cinder/store"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command and returns the process exit code: 0 success, 1 a
// store failure (including a missing key), 2 bad usage.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cinder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "cinder-data", "store directory")
	syncPolicy := fs.String("sync", "always", "sync policy: always or never")
	fs.Usage = func() { usage(stderr) }

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}

	policy, err := parseSync(*syncPolicy)
	if err != nil {
		fmt.Fprintln(stderr, "cinder:", err)
		return 2
	}

	cmd, rest := fs.Arg(0), fs.Args()[1:]
	switch cmd {
	case "set", "get", "del", "compact":
	case "help", "-h", "--help":
		fs.Usage()
		return 0
	default:
		fmt.Fprintf(stderr, "cinder: unknown command %q\n", cmd)
		fs.Usage()
		return 2
	}

	s, err := store.Open(store.Options{Dir: *dir, Sync: policy})
	if err != nil {
		fmt.Fprintln(stderr, "cinder:", err)
		return 1
	}
	defer s.Close()

	switch cmd {
	case "set":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "cinder: set needs a key and a value")
			return 2
		}
		key, value := rest[0], strings.Join(rest[1:], " ")
		if err := s.Put(key, []byte(value)); err != nil {
			fmt.Fprintln(stderr, "cinder:", err)
			return 1
		}
		return 0

	case "get":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "cinder: get needs exactly one key")
			return 2
		}
		value, err := s.Get(rest[0])
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "cinder: key not found: %s\n", rest[0])
			return 1
		}
		if err != nil {
			fmt.Fprintln(stderr, "cinder:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(value))
		return 0

	case "del":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "cinder: del needs exactly one key")
			return 2
		}
		if err := s.Delete(rest[0]); err != nil {
			fmt.Fprintln(stderr, "cinder:", err)
			return 1
		}
		return 0

	default: // compact
		if len(rest) != 0 {
			fmt.Fprintln(stderr, "cinder: compact takes no arguments")
			return 2
		}
		if err := s.Compact(); err != nil {
			fmt.Fprintln(stderr, "cinder:", err)
			return 1
		}
		fmt.Fprintf(stdout, "compacted %s\n", *dir)
		return 0
	}
}

func parseSync(s string) (store.SyncPolicy, error) {
	switch s {
	case "always":
		return store.SyncAlways, nil
	case "never":
		return store.SyncNever, nil
	default:
		return 0, fmt.Errorf("unknown sync policy %q: use always or never", s)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage: cinder [-dir DIR] [-sync always|never] <command>

commands:
  set <key> <value...>   store a value
  get <key>              print a value
  del <key>              delete a key
  compact                rewrite the log, dropping dead records`)
}
