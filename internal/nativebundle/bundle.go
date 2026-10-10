// Package nativebundle resolves and stages native libraries embedded in release builds.
// Ordinary Go builds use host libraries, without bundling native artifacts.
package nativebundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrABIMismatch = errors.New("libkrun/libkrunfw ABI mismatch")

const requiredFirmwareABI = 5

var errCorruptFile = errors.New("untrusted native cache file")

// ValidateABI reports a distinguishable mismatch before libkrun enters the VM.
func ValidateABI(actual int) error { return checkABI(requiredFirmwareABI, actual) }

// Info describes the selected native runtime. A verified path always refers
// to the exact embedded bytes, even if a system library has the same name.
type Info struct {
	Source         string `json:"source"`
	Library        string `json:"library"`
	Firmware       string `json:"firmware,omitempty"`
	LibrarySHA256  string `json:"library_sha256,omitempty"`
	FirmwareSHA256 string `json:"firmware_sha256,omitempty"`
	Integrity      bool   `json:"integrity"`
	FirmwareABI    int    `json:"firmware_abi,omitempty"`
	Fallback       bool   `json:"fallback,omitempty"`
}

func checkABI(expected, actual int) error {
	if expected != actual {
		return fmt.Errorf("%w: libkrun 1.19.6 requires libkrunfw ABI %d, found ABI %d", ErrABIMismatch, expected, actual)
	}
	return nil
}

func names() (string, string) {
	switch {
	case runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"):
		return "libkrun.so.1", "libkrunfw.so.5"
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "libkrun.dylib", "libkrunfw.5.dylib"
	default:
		return "", ""
	}
}

// Library preserves the original API. All execution paths should call
// Resolve instead so the selected source and hashes can be reported.
func Library(explicit string) (string, error) {
	info, err := Resolve(explicit)
	return info.Library, err
}

// Resolve is the sole native library selection point. An explicit host
// override takes precedence; invalid embedded content never silently falls
// back to an arbitrary system installation.
func Resolve(explicit string) (Info, error) {
	return ResolveWithFallback(explicit, false)
}

func ResolveWithFallback(explicit string, allowHost bool) (Info, error) {
	if explicit != "" {
		slog.Info("krunlet using explicitly configured native library", "path", explicit)
		return Info{Source: "host", Library: explicit}, nil
	}
	libName, fwName := names()
	if libName == "" {
		return Info{}, fmt.Errorf("embedded libkrun not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	lib, libErr := assetRead("assets/" + libName)
	fw, fwErr := assetRead("assets/" + fwName)
	if errors.Is(libErr, fs.ErrNotExist) && errors.Is(fwErr, fs.ErrNotExist) {
		slog.Warn("krunlet binary has no embedded libkrun; using system native library")
		return Info{Source: "host", Fallback: true}, nil
	}
	if libErr != nil || fwErr != nil || len(lib) == 0 || len(fw) == 0 {
		if allowHost {
			slog.Warn("unsafe native bundle overridden by explicit host fallback", "lib_error", libErr, "firmware_error", fwErr)
			return Info{Source: "host", Fallback: true}, nil
		}
		return Info{}, fmt.Errorf("embedded native runtime is incomplete (%s: %v, %s: %v); refusing host fallback", libName, libErr, fwName, fwErr)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return Info{}, fmt.Errorf("native cache location: %w", err)
	}
	info, err := stageBundle(filepath.Join(cacheRoot, "krunlet", "native"), libName, fwName, lib, fw)
	if err != nil && allowHost {
		slog.Warn("unsafe native cache overridden by explicit host fallback", "error", err)
		return Info{Source: "host", Fallback: true}, nil
	}
	return info, err
}

func sha(b []byte) string {
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

// stageBundle is deliberately uncached in-process. Every invocation checks
// integrity, so corrupt files can be repaired even by a long-running daemon.
func stageBundle(root, libName, fwName string, lib, fw []byte) (Info, error) {
	info := Info{
		Source: "embedded", LibrarySHA256: sha(lib), FirmwareSHA256: sha(fw),
		FirmwareABI: requiredFirmwareABI,
	}
	full := sha(append(append([]byte(nil), lib...), fw...))
	if err := os.MkdirAll(root, 0700); err != nil {
		return info, fmt.Errorf("create native cache: %w", err)
	}
	if err := privateDir(root); err != nil {
		return info, err
	}
	release, err := bundleLock(root)
	if err != nil {
		return info, err
	}
	defer release()

	target := filepath.Join(root, full)
	info.Library, info.Firmware = filepath.Join(target, libName), filepath.Join(target, fwName)
	if _, e := os.Lstat(target); e == nil {
		// A symlink, a non-private directory or an individual symlink is
		// suspicious and must not be removed or followed automatically.
		if e := privateDir(target); e != nil {
			return info, e
		}
		valid, e := verifyPair(info, lib, fw)
		if e != nil {
			return info, e
		}
		if valid {
			info.Integrity = true
			return info, nil
		}
		// The old target is unusable. Move it out of the content-addressed
		// location before installing a fully verified replacement.
		retired, e := os.MkdirTemp(root, ".retired-")
		if e != nil {
			return info, e
		}
		if e := os.Remove(retired); e != nil {
			return info, e
		}
		if e := os.Rename(target, retired); e != nil {
			return info, fmt.Errorf("quarantine corrupt native bundle: %w", e)
		}
		defer os.RemoveAll(retired)
	} else if !errors.Is(e, os.ErrNotExist) {
		return info, e
	}
	stage, err := os.MkdirTemp(root, ".stage-")
	if err != nil {
		return info, err
	}
	defer os.RemoveAll(stage)
	for _, f := range []struct {
		name string
		data []byte
	}{{libName, lib}, {fwName, fw}} {
		p := filepath.Join(stage, f.name)
		if err := writeVerified(p, f.data); err != nil {
			return info, err
		}
	}
	if err := syncDir(stage); err != nil {
		return info, err
	}
	if err := os.Rename(stage, target); err != nil {
		return info, fmt.Errorf("atomically publish native bundle: %w", err)
	}
	if err := syncDir(root); err != nil {
		return info, err
	}
	valid, err := verifyPair(info, lib, fw)
	if err != nil {
		return info, err
	}
	if !valid {
		return info, errors.New("embedded bundle contents changed during extraction")
	}
	info.Integrity = true
	return info, nil
}

func writeVerified(path string, expected []byte) error {
	file, err := openNativeFile(path, true)
	if err != nil {
		return err
	}
	if _, err = file.Write(expected); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	got, err := readVerifiedFile(path)
	if err != nil {
		return err
	}
	if sha(got) != sha(expected) {
		return fmt.Errorf("native bundle write checksum mismatch: %s", path)
	}
	return nil
}

func verifyPair(info Info, lib, fw []byte) (bool, error) {
	for _, item := range []struct {
		path string
		data []byte
	}{{info.Library, lib}, {info.Firmware, fw}} {
		got, err := readVerifiedFile(item.path)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errCorruptFile) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if sha(got) != sha(item.data) {
			return false, nil
		}
	}
	return true, nil
}

func readVerifiedFile(path string) ([]byte, error) {
	file, err := openNativeFile(path, false)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0222 != 0 {
		return nil, fmt.Errorf("%w: %s: expected read-only regular file", errCorruptFile, path)
	}
	if stat.Size() < 1 || stat.Size() > 256<<20 {
		return nil, fmt.Errorf("invalid native file size: %s", path)
	}
	return io.ReadAll(file)
}

func privateDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("unsafe native cache directory %s: expected real directory with 0700 permissions", path)
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !strings.Contains(err.Error(), "invalid argument") {
		return err
	}
	return nil
}
