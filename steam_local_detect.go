package main

import (
	"regexp"
	"strings"
)

// Pure-Go parsers for Steam VDF files. Regex-based rather than a full
// VDF parser since these files are Steam-generated with a stable shape
// and we only need a couple of fields.

var acfNameRe = regexp.MustCompile(`"name"\s+"([^"]+)"`)

func extractACFName(content string) string {
	m := acfNameRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[1])
}

// extractVDFPaths normalises paths from libraryfolders.vdf to backslash
// form regardless of which flavour Steam wrote (escaped backslashes or
// forward slashes). filepath.FromSlash would be a no-op on non-Windows
// test runners and mask the conversion in tests.
var vdfPathRe = regexp.MustCompile(`"path"\s+"([^"]+)"`)

func extractVDFPaths(content string) []string {
	matches := vdfPathRe.FindAllStringSubmatch(content, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		p := strings.ReplaceAll(m[1], `\\`, `\`)
		p = strings.ReplaceAll(p, `/`, `\`)
		out = append(out, p)
	}
	return out
}
