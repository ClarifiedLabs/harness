package acp

import (
	"fmt"
	"slices"
)

// Version is an ACP major protocol version.
type Version uint16

const (
	// ProtocolVersion is the latest stable ACP version implemented here.
	ProtocolVersion Version = 1
)

// SupportedVersions lists supported ACP versions, newest first.
var SupportedVersions = []Version{ProtocolVersion}

// Supports reports whether version is implemented by this package.
func Supports(version Version) bool {
	return slices.Contains(SupportedVersions, version)
}

// IncompatibleVersionError reports that a peer selected or requested an ACP
// version this package cannot speak. Selected is the version a v1 agent must
// put in its initialize response when Offered is unsupported.
type IncompatibleVersionError struct {
	Offered   Version
	Selected  Version
	Supported []Version
}

func (e *IncompatibleVersionError) Error() string {
	return fmt.Sprintf("acp: incompatible protocol version %d (supported: %v)", e.Offered, e.Supported)
}

// NegotiateVersion selects an ACP version. ACP peers offer their latest version;
// if it is unsupported, an agent must answer with its own latest version. The
// returned selected version is therefore useful even when err is non-nil. A
// client receiving that error should close the connection.
func NegotiateVersion(offered Version) (selected Version, err error) {
	if Supports(offered) {
		return offered, nil
	}
	selected = ProtocolVersion
	return selected, &IncompatibleVersionError{
		Offered:   offered,
		Selected:  selected,
		Supported: slices.Clone(SupportedVersions),
	}
}
