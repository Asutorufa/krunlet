//go:build nokrunlet_embed

package nativebundle

import (
	"io/fs"
)

func assetRead(string) ([]byte, error) { return nil, fs.ErrNotExist }
