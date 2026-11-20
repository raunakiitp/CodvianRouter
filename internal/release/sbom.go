package release

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Module is one record of `go list -m -json all` output: the main module
// (Main == true, no Version) plus every dependency. Replace records a
// `replace` directive: when present, the replacement's path/version is
// what actually ends up in the build.
type Module struct {
	Path    string  `json:"Path"`
	Version string  `json:"Version"`
	Main    bool    `json:"Main"`
	Replace *Module `json:"Replace"`
}

// ParseGoListJSON parses the concatenated JSON objects `go list -m -json
// all` writes to stdout. It stops cleanly at EOF and rejects trailing
// garbage, so a truncated module list cannot silently produce a partial
// SBOM.
func ParseGoListJSON(r io.Reader) ([]Module, error) {
	dec := json.NewDecoder(r)
	var mods []Module
	for {
		var m Module
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("release: parsing go list output: %w", err)
		}
		if m.Path == "" {
			return nil, fmt.Errorf("release: parsing go list output: module record with empty Path")
		}
		mods = append(mods, m)
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("release: parsing go list output: no modules found")
	}
	return mods, nil
}

// effectiveVersion is the version that actually ships for a module: the
// replacement's version when a replace directive exists, else the
// module's own version. A replace with no version is a local-path
// replace — the original version does NOT ship, so the result is ""
// (unknown), never the replaced-away version.
func effectiveVersion(m Module) string {
	if m.Replace != nil {
		return m.Replace.Version
	}
	return m.Version
}

// effectivePath is the path that actually ships for a module.
func effectivePath(m Module) string {
	if m.Replace != nil && m.Replace.Path != "" {
		return m.Replace.Path
	}
	return m.Path
}

// purl renders the Package URL for a Go module. The version qualifier is
// omitted when no version is known, rather than inventing one.
func purl(path, version string) string {
	if version == "" {
		return "pkg:golang/" + path
	}
	return "pkg:golang/" + path + "@" + version
}

type cyclonedxComponent struct {
	Type    string `json:"type"`
	BOMRef  string `json:"bom-ref"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	PURL    string `json:"purl"`
}

type cyclonedxTool struct {
	Vendor  string `json:"vendor"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type cyclonedxMetadata struct {
	Timestamp string             `json:"timestamp"`
	Tools     []cyclonedxTool    `json:"tools"`
	Component cyclonedxComponent `json:"component"`
}

type cyclonedxSBOM struct {
	BOMFormat   string               `json:"bomFormat"`
	SpecVersion string               `json:"specVersion"`
	Serial      string               `json:"serialNumber"`
	Version     int                  `json:"version"`
	Metadata    cyclonedxMetadata    `json:"metadata"`
	Components  []cyclonedxComponent `json:"components"`
}

// GenerateSBOM renders a CycloneDX 1.6 SBOM (JSON) for a Go module
// graph: rootPath/rootVersion is the project's own module (the metadata
// component), deps are every other module from `go list -m -json all`
// (the main module is never listed twice). Components are sorted by
// purl and deduplicated by path, so the same graph always yields the
// same component list. The serialNumber is a fresh random UUID per
// render and the timestamp is now — an SBOM describes one build, not a
// reproducible byte stream, so those two fields are intentionally not
// stable. rootVersion "" is rendered as "0.0.0-dev": an unreleased tree
// has no version, and inventing one would be worse than saying so.
func GenerateSBOM(rootPath, rootVersion string, deps []Module) ([]byte, error) {
	if strings.TrimSpace(rootPath) == "" {
		return nil, fmt.Errorf("release: SBOM root module path is empty")
	}
	if rootVersion == "" {
		rootVersion = "0.0.0-dev"
	}
	byPath := make(map[string]cyclonedxComponent)
	for _, m := range deps {
		if m.Main {
			continue
		}
		path := effectivePath(m)
		version := effectiveVersion(m)
		if _, dup := byPath[path]; dup {
			continue
		}
		byPath[path] = cyclonedxComponent{
			Type:    "library",
			BOMRef:  purl(path, version),
			Name:    path,
			Version: version,
			PURL:    purl(path, version),
		}
	}
	components := make([]cyclonedxComponent, 0, len(byPath))
	for _, c := range byPath {
		components = append(components, c)
	}
	sort.Slice(components, func(i, j int) bool { return components[i].PURL < components[j].PURL })

	uuid, err := newUUID()
	if err != nil {
		return nil, fmt.Errorf("release: generating SBOM serial number: %w", err)
	}
	sbom := cyclonedxSBOM{
		BOMFormat:   "CycloneDX",
		SpecVersion: "1.6",
		Serial:      "urn:uuid:" + uuid,
		Version:     1,
		Metadata: cyclonedxMetadata{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Tools: []cyclonedxTool{{
				Vendor:  "Codvian",
				Name:    "codvian-router release-sbom",
				Version: rootVersion,
			}},
			Component: cyclonedxComponent{
				Type:    "application",
				BOMRef:  purl(rootPath, rootVersion),
				Name:    rootPath,
				Version: rootVersion,
				PURL:    purl(rootPath, rootVersion),
			},
		},
		Components: components,
	}
	out, err := json.MarshalIndent(sbom, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("release: encoding SBOM: %w", err)
	}
	return append(out, '\n'), nil
}

// newUUID returns a random RFC 4122 version-4 UUID string.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
