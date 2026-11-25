package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func writePkg(t *testing.T, dir, pkgJSON string) {
	t.Helper()
	files := map[string]string{
		"package.json":      pkgJSON,
		"package-lock.json": `{"name":"x","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"x","version":"1.0.0"}}}`,
		"index.js":          `export default {}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A package id that climbs out of packages/ must be refused before anything
// is deleted.
func TestInstallRefusesTraversingID(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(victim, "precious.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../../victim", "/abs/path", "a/../../victim", "Upper", ""} {
		src := t.TempDir()
		name := id
		if name == "" {
			name = "x"
		}
		writePkg(t, src, `{"name":"`+name+`","version":"1.0.0","type":"module","crabbro":{"id":"`+id+`","api":"1.0.0","entry":"index.js"}}`)
		m, err := NewManager(filepath.Join(root, "store"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if id != "" {
			if _, err := m.Install(src, true); err == nil {
				t.Errorf("id %q installed", id)
			}
		}
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("id %q deleted a directory outside the store: %v", id, err)
		}
	}
}

func TestInstallRefusesUnknownPermission(t *testing.T) {
	src := t.TempDir()
	writePkg(t, src, `{"name":"p","version":"1.0.0","type":"module","crabbro":{"id":"p","api":"1.0.0","entry":"index.js","permissions":["shell"]}}`)
	m, err := NewManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(src, true); err == nil {
		t.Error("unknown permission accepted")
	}
}

// Native addons come in with dependencies, so the scan has to look at the
// installed tree, not the source.
func TestRejectNativeFindsInstalledAddon(t *testing.T) {
	dir := t.TempDir()
	nm := filepath.Join(dir, "node_modules", "dep", "build")
	if err := os.MkdirAll(nm, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "dep.node"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectNative(dir, nil); err == nil {
		t.Error("addon under node_modules not refused")
	}
	if err := rejectNative(dir, []string{"native"}); err != nil {
		t.Errorf("granted native refused: %v", err)
	}
}

func TestProviderManifestValidation(t *testing.T) {
	cases := []struct {
		name, providers string
		valid           bool
	}{
		{"valid", `[{"id":"partner","displayName":"Partner","defaultBaseUrl":"https://example.com/v1","defaultEndpoints":["openai-chat"],"defaultModels":["model"],"authMethods":[{"kind":"plugin-oauth"},{"kind":"plugin-token"}]}]`, true},
		{"builtin collision", `[{"id":"openai","displayName":"Fake","authMethods":[{"kind":"plugin-token"}]}]`, false},
		{"duplicate", `[{"id":"partner","displayName":"P","authMethods":[{"kind":"plugin-token"}]},{"id":"partner","displayName":"P","authMethods":[{"kind":"plugin-token"}]}]`, false},
		{"bad id", `[{"id":"Bad ID","displayName":"P","authMethods":[{"kind":"plugin-token"}]}]`, false},
		{"empty name", `[{"id":"partner","authMethods":[{"kind":"plugin-token"}]}]`, false},
		{"missing auth", `[{"id":"partner","displayName":"P"}]`, false},
		{"invalid url", `[{"id":"partner","displayName":"P","defaultBaseUrl":"https://user:pass@example.com","authMethods":[{"kind":"plugin-token"}]}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePkg(t, dir, `{"name":"p","version":"1.0.0","crabbro":{"id":"p","api":"1.0.0","entry":"index.js","providers":`+tc.providers+`}}`)
			meta, err := readPackage(dir)
			if (err == nil) != tc.valid {
				t.Fatalf("readPackage error: %v", err)
			}
			if tc.valid && (len(meta.Providers) != 1 || string(meta.Providers[0].SupportedAuth[0].Kind) != "plugin-oauth") {
				t.Fatalf("declaration lost: %+v", meta.Providers)
			}
		})
	}
}
