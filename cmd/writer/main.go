// Command writer fills a Cinder store as fast as it can and prints each key to
// stdout only after Put has returned.
//
// It is meant to be killed by cmd/crashtest at a random moment. The newline
// after the key is the acknowledgement: if the harness read it, Put returned,
// and the record must survive.
package main

import (
	"flag"
	"fmt"
	"os"

	"cinder/internal/crashproto"
	"cinder/store"
)

func main() {
	dir := flag.String("dir", "data", "store directory")
	policy := flag.String("sync", "always", "sync policy: always or never")
	start := flag.Int("start", 0, "first key index")
	flag.Parse()

	var syncPolicy store.SyncPolicy
	switch *policy {
	case "always":
		syncPolicy = store.SyncAlways
	case "never":
		syncPolicy = store.SyncNever
	default:
		fmt.Fprintf(os.Stderr, "writer: unknown sync policy %q\n", *policy)
		os.Exit(2)
	}

	s, err := store.Open(store.Options{Dir: *dir, Sync: syncPolicy})
	if err != nil {
		fmt.Fprintf(os.Stderr, "writer: %v\n", err)
		os.Exit(1)
	}

	// No Close: the harness kills this process on purpose.
	for i := *start; ; i++ {
		key := crashproto.Key(i)
		if err := s.Put(key, []byte(crashproto.Value(key))); err != nil {
			fmt.Fprintf(os.Stderr, "writer: put %s: %v\n", key, err)
			os.Exit(1)
		}
		// Unbuffered write: the line leaves the process after Put returned.
		if _, err := fmt.Fprintln(os.Stdout, key); err != nil {
			os.Exit(1)
		}
	}
}
