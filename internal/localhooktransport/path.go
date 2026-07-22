package localhooktransport

import (
	"path/filepath"
	"strings"
)

func validateSocketPathSyntax(socketPath string) error {
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return ErrInsecureSocketPath
	}
	for _, component := range strings.Split(socketPath, string(filepath.Separator)) {
		if component == ".." {
			return ErrInsecureSocketPath
		}
	}
	if filepath.Base(socketPath) == "." || filepath.Base(socketPath) == string(filepath.Separator) {
		return ErrInsecureSocketPath
	}
	return nil
}
