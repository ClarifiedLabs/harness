package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalBackgroundResourceResolvesExistingSymlinkPrefix(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir real resource: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink resource: %v", err)
	}

	got, err := CanonicalBackgroundResource(filepath.Join(link, "future", "..", "child"))
	if err != nil {
		t.Fatalf("CanonicalBackgroundResource: %v", err)
	}
	canonicalReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks real resource: %v", err)
	}
	want := filepath.Join(canonicalReal, "child")
	if got != want {
		t.Fatalf("canonical resource = %q, want %q", got, want)
	}
}

func TestResolveBackgroundLeaseDefaultsAndValidation(t *testing.T) {
	defaultResource := t.TempDir()
	gotResource, gotAccess, err := ResolveBackgroundLease(
		"",
		"",
		defaultResource,
		BackgroundAccessExclusive,
	)
	if err != nil {
		t.Fatalf("ResolveBackgroundLease: %v", err)
	}
	wantResource, err := filepath.EvalSymlinks(defaultResource)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if gotResource != wantResource || gotAccess != BackgroundAccessExclusive {
		t.Fatalf("resolved lease = %q/%q, want %q/%q", gotResource, gotAccess, wantResource, BackgroundAccessExclusive)
	}

	for _, test := range []struct {
		name     string
		resource string
		access   string
	}{
		{name: "missing resource", access: BackgroundAccessReadOnly},
		{name: "missing access", resource: defaultResource},
		{name: "invalid access", resource: defaultResource, access: "shared"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := NormalizeBackgroundLease(test.resource, test.access); err == nil {
				t.Fatal("NormalizeBackgroundLease error = nil")
			}
		})
	}
}
