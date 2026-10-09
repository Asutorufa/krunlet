package krunlet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Reject traversal and all host symlink paths used for input/output exchange.
func guestPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.IndexByte(p, 0) >= 0 {
		return "", errors.New("guest path must be absolute and NUL-free")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errors.New("parent directory traversal disallowed")
		}
	}
	clean := path.Clean(p)
	if clean == "/" {
		return clean, nil
	}
	return clean, nil
}
func checkedHostPath(root, guest string) (string, error) {
	p, e := guestPath(guest)
	if e != nil {
		return "", e
	}
	if p == "/" {
		return "", errors.New("cannot exchange root directory")
	}
	name := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	// Verify every already-existing path component (not just the leaf).
	rel := strings.TrimPrefix(p, "/")
	here := root
	for _, part := range strings.Split(rel, "/") {
		here = filepath.Join(here, part)
		fi, e := os.Lstat(here)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", e
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink path component forbidden: %s", here)
		}
	}
	return name, nil
}

func copyRootFS(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		to := filepath.Join(dst, rel)
		fi, e := d.Info()
		if e != nil {
			return e
		}
		switch {
		case d.IsDir():
			return os.Mkdir(to, fi.Mode().Perm()|0700)
		case fi.Mode().IsRegular():
			in, e := os.Open(p)
			if e != nil {
				return e
			}
			out, e := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
			if e != nil {
				_ = in.Close()
				return e
			}
			_, copyErr := io.Copy(out, &contextReader{ctx: ctx, reader: in})
			closeErr := out.Close()
			inClose := in.Close()
			if copyErr != nil { return copyErr }
			if closeErr != nil { return closeErr }
			return inClose
		case fi.Mode()&os.ModeSymlink != 0:
			target, e := os.Readlink(p)
			if e != nil {
				return e
			}
			return os.Symlink(target, to)
		default:
			return fmt.Errorf("unsupported special file in rootfs: %s", p)
		}
	})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (c *contextReader) Read(b []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.reader.Read(b)
}

func validatePortMap(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid port map %q", s)
	}
	for _, p := range parts {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port in %q", s)
		}
	}
	return nil
}
func validateRLimit(s string) error {
	eq := strings.Split(s, "=")
	if len(eq) != 2 {
		return fmt.Errorf("invalid rlimit %q", s)
	}
	if _, e := strconv.ParseUint(eq[0], 10, 32); e != nil {
		return fmt.Errorf("invalid resource in %q", s)
	}
	vals := strings.Split(eq[1], ":")
	if len(vals) != 2 {
		return fmt.Errorf("invalid rlimit %q", s)
	}
	for _, v := range vals {
		if _, e := strconv.ParseUint(v, 10, 64); e != nil {
			return fmt.Errorf("invalid rlimit %q", s)
		}
	}
	return nil
}
