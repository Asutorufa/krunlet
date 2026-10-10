//go:build !linux && !darwin

package nativebundle

import (
	"errors"
	"os"
)

func bundleLock(string) (func(), error) { return nil, errors.New("unsupported platform") }
func openNativeFile(string, bool) (*os.File, error) { return nil, errors.New("unsupported platform") }
