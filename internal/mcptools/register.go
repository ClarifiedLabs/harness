package mcptools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"harness/internal/mcp"
	"harness/internal/tools"
)

// namePrefix is the namespace every proxy tool name must carry. The proxy
// builds names as mcp__<server>__<tool>; the harness validates and registers
// them under that exact prefix.
const namePrefix = "mcp__"

// toolNameRe is the provider-imposed tool-name charset and length bound
// ([a-zA-Z0-9_-], 1..64). A name that fails it cannot be sent to the model, so
// the tool is skipped.
var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Summary reports the outcome of a Register pass.
type Summary struct {
	Servers       map[string]int // display-only server name -> tool count
	Skipped       []string       // names skipped for failing validation
	Names         []string       // full names of tools registered, in list order
	ReadOnlyNames []string       // registered names whose adapted tool is trusted read-only
	Total         int            // count of tools registered
}

// RegisterOptions controls how proxy-discovered tools are adapted. By default
// MCP annotations are treated as untrusted and do not affect scheduling.
type RegisterOptions struct {
	TrustReadOnlyHint bool
}

// Register lists the proxy's tools and registers each valid one on reg as an
// *mcptools.Tool backed by conn. Names are validated against the provider
// charset and the required mcp__ prefix; invalid names are skipped and recorded.
// A later Register replaces same-named tools in place (Registry.Register
// semantics), so refresh can re-run it; the returned Names let the
// caller compute removals against a previous set.
func Register(ctx context.Context, reg *tools.Registry, conn *Conn) (Summary, error) {
	return RegisterWithOptions(ctx, reg, conn, RegisterOptions{})
}

// RegisterWithOptions is Register plus adapter policy for MCP read-only hints.
func RegisterWithOptions(ctx context.Context, reg *tools.Registry, conn *Conn, opts RegisterOptions) (Summary, error) {
	defs, err := conn.ListTools(ctx)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{Servers: make(map[string]int)}
	for _, d := range defs {
		if !validName(d.Name) {
			sum.Skipped = append(sum.Skipped, d.Name)
			continue
		}
		readOnly := opts.TrustReadOnlyHint && readOnlyHint(d)
		reg.Register(&Tool{
			name:     d.Name,
			desc:     oneLineDesc(d.Description),
			schema:   normalizeSchema(d.InputSchema),
			readOnly: readOnly,
			conn:     conn,
		})
		sum.Names = append(sum.Names, d.Name)
		if readOnly {
			sum.ReadOnlyNames = append(sum.ReadOnlyNames, d.Name)
		}
		sum.Servers[serverLabel(d.Name)]++
		sum.Total++
	}
	return sum, nil
}

func readOnlyHint(d mcp.Tool) bool {
	if len(d.Annotations) == 0 {
		return false
	}
	var annotations struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	}
	if err := json.Unmarshal(d.Annotations, &annotations); err != nil {
		return false
	}
	return annotations.ReadOnlyHint
}

// validName reports whether name is a registrable MCP tool name: it must carry
// the mcp__ prefix (defensive against a misbehaving proxy emitting bare names)
// and match the provider charset/length bound.
func validName(name string) bool {
	return strings.HasPrefix(name, namePrefix) && toolNameRe.MatchString(name)
}

// serverLabel extracts a display-only server label from a validated name. The
// name is mcp__<server>__<tool>, but a server name may itself contain "__", so
// without the proxy's routing table the split is ambiguous. The label is the
// segment up to the FIRST "__" after the prefix: a best-effort display value
// only, never used for routing.
func serverLabel(name string) string {
	rest := strings.TrimPrefix(name, namePrefix)
	label, _, _ := strings.Cut(rest, "__")
	return label
}
