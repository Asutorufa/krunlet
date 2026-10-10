//go:build !nokrunlet_embed

package nativebundle

import "embed"

//go:embed assets/*
var embeddedAssets embed.FS

func assetRead(path string) ([]byte, error) { return embeddedAssets.ReadFile(path) }
