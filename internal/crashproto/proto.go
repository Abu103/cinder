// Package crashproto is the contract between cmd/writer and cmd/crashtest.
// The writer prints a key only after Put has returned; the harness derives the
// expected value from the key alone, so it never has to trust the child's
// output beyond the key name.
package crashproto

import "fmt"

// Key is the key written at position i.
func Key(i int) string { return fmt.Sprintf("k%d", i) }

// Value is the (deterministic) value stored for key.
func Value(key string) string { return "value-" + key }
