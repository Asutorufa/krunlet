// Package nativebundle provides optional build-time native assets.
// Plain go build and go install continue using system-installed libraries.
package nativebundle

import (
    "bytes"
    "crypto/sha256"
    "embed"
    "errors"
    "fmt"
    "io/fs"
    "os"
    "path/filepath"
    "runtime"
    "sync"
)

//go:embed assets/*
var assets embed.FS

var cache struct {
    sync.Once
    path string
    err error
}

// Library honors explicit overrides. A complete embedded release bundle is
// used by default; otherwise the system dynamic loader is used as before.
// Incomplete or modified bundles fail closed instead of silently falling back.
func Library(explicit string) (string, error) {
    if explicit != "" { return explicit, nil }
    cache.Do(func() { cache.path, cache.err = materialize() })
    return cache.path, cache.err
}

func names() (string, string) {
    switch {
    case runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"):
        return "libkrun.so.1", "libkrunfw.so.5"
    case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
        return "libkrun.dylib", "libkrunfw.dylib"
    default:
        return "", ""
    }
}

func materialize() (string, error) {
    libName, fwName := names()
    if libName == "" { return "", nil }
    lib, libErr := assets.ReadFile("assets/" + libName)
    fw, fwErr := assets.ReadFile("assets/" + fwName)
    if errors.Is(libErr, fs.ErrNotExist) && errors.Is(fwErr, fs.ErrNotExist) {
        return "", nil
    }
    if libErr != nil || fwErr != nil || len(lib) == 0 || len(fw) == 0 {
        return "", fmt.Errorf("incomplete native bundle (%s: %v, %s: %v)", libName, libErr, fwName, fwErr)
    }
    h := sha256.New()
    _, _ = h.Write(lib)
    _, _ = h.Write(fw)
    key := fmt.Sprintf("%x", h.Sum(nil))
    home, err := os.UserCacheDir()
    if err != nil { return "", err }
    parent := filepath.Join(home, "krunlet", "native")
    if err := os.MkdirAll(parent, 0700); err != nil { return "", err }
    if err := privateDir(parent); err != nil { return "", err }
    target := filepath.Join(parent, key)
    if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
        tmp, err := os.MkdirTemp(parent, ".stage-")
        if err != nil { return "", err }
        defer os.RemoveAll(tmp)
        if err := os.WriteFile(filepath.Join(tmp, libName), lib, 0500); err != nil { return "", err }
        if err := os.WriteFile(filepath.Join(tmp, fwName), fw, 0500); err != nil { return "", err }
        if err := os.Rename(tmp, target); err != nil {
            if _, statErr := os.Lstat(target); statErr != nil { return "", err }
            // Another process may have installed an identical bundle.
        }
    } else if err != nil {
        return "", err
    }
    if err := privateDir(target); err != nil { return "", err }
    for name, expected := range map[string][]byte{libName: lib, fwName: fw} {
        p := filepath.Join(target, name)
        fi, err := os.Lstat(p)
        if err != nil { return "", err }
        if !fi.Mode().IsRegular() || fi.Mode().Perm()&0022 != 0 {
            return "", fmt.Errorf("unsafe cached native library: %s", p)
        }
        got, err := os.ReadFile(p)
        if err != nil { return "", err }
        if !bytes.Equal(got, expected) {
            return "", fmt.Errorf("cached native library checksum mismatch: %s (remove corrupt cache)", p)
        }
    }
    return filepath.Join(target, libName), nil
}

func privateDir(path string) error {
    fi, err := os.Lstat(path)
    if err != nil { return err }
    if !fi.IsDir() || fi.Mode().Perm()&0077 != 0 {
        return fmt.Errorf("unsafe native bundle cache directory: %s (requires private 0700 directory)", path)
    }
    return nil
}
