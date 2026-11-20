// Command crashtest kills a writer mid-write, over and over, and checks that
// every key the writer acknowledged is still readable after recovery.
//
// An acknowledgement is a complete line on the child's stdout: cmd/writer
// prints a key only after Put returned. A partial line is discarded, because we
// cannot know whether that Put returned.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

func main() {
	runs := flag.Int("runs", 200, "number of kill/verify cycles")
	minDelay := flag.Int("min-delay-ms", 5, "shortest time the writer may live")
	maxDelay := flag.Int("max-delay-ms", 500, "longest time the writer may live")
	policy := flag.String("sync", "always", "sync policy for the writer: always or never")
	verbose := flag.Bool("v", false, "print the acknowledged key count of every run")
	keep := flag.Bool("keep", false, "keep the data directories for inspection")
	flag.Parse()

	if *runs < 1 {
		fmt.Fprintln(os.Stderr, "crashtest: -runs must be at least 1")
		os.Exit(2)
	}
	if *maxDelay <= *minDelay {
		fmt.Fprintln(os.Stderr, "crashtest: -max-delay-ms must be greater than -min-delay-ms")
		os.Exit(2)
	}
	if *policy != "always" && *policy != "never" {
		fmt.Fprintf(os.Stderr, "crashtest: unknown -sync %q\n", *policy)
		os.Exit(2)
	}

	writer, err := buildWriter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "crashtest:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(filepath.Dir(writer))

	fmt.Printf("cinder crashtest: %d runs, kills after %d-%d ms, sync=%s\n",
		*runs, *minDelay, *maxDelay, *policy)
	fmt.Println("note: Process.Kill tests process crashes; only a power cut tests fsync.")

	started := time.Now()
	passed, failed := 0, 0
	for i := 1; i <= *runs; i++ {
		dir, err := os.MkdirTemp("", "cinder-crash-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "crashtest:", err)
			os.Exit(1)
		}

		acked, err := runOnce(writer, dir, *policy, *minDelay, *maxDelay)
		if err != nil {
			failed++
			fmt.Printf("run %3d/%d FAILED to run writer: %v (data kept in %s)\n", i, *runs, err, dir)
			continue
		}
		if err := verify(filepath.Join(dir, "data"), acked); err != nil {
			failed++
			fmt.Printf("run %3d/%d LOST DATA after %d acknowledged keys: %v (data kept in %s)\n",
				i, *runs, len(acked), err, dir)
			continue
		}

		passed++
		if *verbose {
			fmt.Printf("run %3d/%d ok, %d keys acknowledged\n", i, *runs, len(acked))
		}
		if !*keep {
			_ = os.RemoveAll(dir)
		}
	}

	fmt.Printf("%d/%d passed in %s\n", passed, *runs, time.Since(started).Round(time.Millisecond))
	if failed > 0 {
		os.Exit(1)
	}
}

// runOnce starts the writer, kills it after a random delay, and returns the
// keys it acknowledged before it died.
func runOnce(writer, dir, policy string, minDelay, maxDelay int) ([]string, error) {
	cmd := exec.Command(writer, "-dir", filepath.Join(dir, "data"), "-sync", policy)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start writer: %w", err)
	}

	var (
		mu    sync.Mutex
		acked []string
	)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		r := bufio.NewReader(stdout)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				// EOF, or a half-written last line. Neither is acknowledged.
				return
			}
			key := strings.TrimSpace(line)
			if key == "" {
				continue
			}
			mu.Lock()
			acked = append(acked, key)
			mu.Unlock()
		}
	}()

	time.Sleep(time.Duration(minDelay+rand.Intn(maxDelay-minDelay)) * time.Millisecond)
	_ = cmd.Process.Kill()

	// All reads must finish before Wait: Wait closes the pipe.
	<-readDone
	if err := cmd.Wait(); err != nil {
		// The process was killed; that is the point. Only report a failure to
		// start or an error that came with output on stderr and no keys.
		if len(acked) == 0 && stderr.Len() > 0 {
			return nil, fmt.Errorf("writer exited: %v: %s", err, stderr.String())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), acked...), nil
}

// buildWriter builds cmd/writer once so the harness kills the real binary.
// `go run` would leave the compiled child running after the parent is killed.
func buildWriter() (string, error) {
	dir, err := os.MkdirTemp("", "cinder-writer-*")
	if err != nil {
		return "", err
	}
	name := "writer"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)

	var stderr bytes.Buffer
	cmd := exec.Command("go", "build", "-o", out, "./cmd/writer")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("build writer: %w: %s", err, stderr.String())
	}
	return out, nil
}
