package nativebundle

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
)

func TestExplicitOverride(t *testing.T) {
    path, err := Library("/example/libkrun.so.1")
    if err != nil || path != "/example/libkrun.so.1" { t.Fatalf("override: %q %v", path, err) }
}

func TestBundledOrSystem(t *testing.T) {
    lib, err := Library("")
    if err != nil { t.Fatal(err) }
    if lib == "" { return }
    if !filepath.IsAbs(lib) || !strings.HasPrefix(filepath.Base(lib), "libkrun") {
        t.Fatalf("unexpected bundled lib: %q", lib)
    }
    if _, err := os.Stat(lib); err != nil { t.Fatal(err) }
    _, fw := names()
    if _, err := os.Stat(filepath.Join(filepath.Dir(lib), fw)); err != nil { t.Fatal(err) }
    second, err := Library("")
    if err != nil || second != lib { t.Fatalf("cache resolution: %s %v", second, err) }
}

func TestPrivateDirRejectsSymlinksAndLoosePermissions(t *testing.T) {
    dir := t.TempDir()
    loose := filepath.Join(dir, "loose")
    if err := os.Mkdir(loose, 0755); err != nil { t.Fatal(err) }
    if err := privateDir(loose); err == nil { t.Fatal("accepted world-readable cache") }
    link := filepath.Join(dir, "link")
    if err := os.Symlink(loose, link); err != nil { t.Fatal(err) }
    if err := privateDir(link); err == nil { t.Fatal("accepted symlink") }
}
