//go:build linux

package nativebundle

import (
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

func checkGlibcBaseline(baseline string) error {
	if baseline == "" {
		return nil
	}
	h, err := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return fmt.Errorf("cannot inspect glibc: %w", err)
	}
	defer purego.Dlclose(h)
	s, err := purego.Dlsym(h, "gnu_get_libc_version")
	if err != nil {
		return fmt.Errorf("not glibc: %w", err)
	}
	var f func() unsafe.Pointer
	purego.RegisterFunc(&f, s)
	p := f()
	if p == nil {
		return fmt.Errorf("glibc reported empty version")
	}
	var v []byte
	for i := uintptr(0); i < 32; i++ {
		ch := *(*byte)(unsafe.Add(p, i))
		if ch == 0 {
			break
		}
		v = append(v, ch)
	}
	found := string(v)
	cmp := func(s string) (int, int, error) {
		p := strings.Split(s, ".")
		if len(p) < 2 {
			return 0, 0, fmt.Errorf("invalid glibc version %q", s)
		}
		a, e := strconv.Atoi(p[0])
		if e != nil {
			return 0, 0, e
		}
		b, e := strconv.Atoi(p[1])
		return a, b, e
	}
	maj, min, err := cmp(found)
	if err != nil {
		return err
	}
	reqMaj, reqMin, err := cmp(baseline)
	if err != nil {
		return err
	}
	if maj < reqMaj || maj == reqMaj && min < reqMin {
		return fmt.Errorf("your glibc version %s is older than this build's minimum %s", found, baseline)
	}
	return nil
}
