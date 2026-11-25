package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreTightensExistingDirectory(t *testing.T) {
	for _, operation := range []string{"set", "get", "list"} {
		t.Run(operation, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "secrets")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "key"), []byte("value"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			s := New(dir)
			var err error
			switch operation {
			case "set":
				err = s.Set("key", "value")
			case "get":
				_, err = s.Get("key")
			case "list":
				_, err = s.List()
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o700 {
				t.Fatalf("directory mode = %o, want 0700", got)
			}
		})
	}
}
