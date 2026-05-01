package replacedlib

import (
	_ "embed"
	"strings"
)

//go:embed asset.txt
var asset string

func Asset() string {
	return strings.TrimSpace(asset)
}
