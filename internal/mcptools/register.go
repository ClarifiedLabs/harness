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
	// Namespace adapts bare downstream tool names into mcp__<namespace>__<tool>
	// names. Empty preserves the default proxy behavior, where the downstream
	// must already advertise fully-qualified mcp__ names.
	Namespace string
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
		name, target, ok := registrationNames(d.Name, opts.Namespace)
		if !ok {
			sum.Skipped = append(sum.Skipped, d.Name)
			continue
		}
		readOnly := opts.TrustReadOnlyHint && readOnlyHint(d)
		reg.Register(&Tool{
			name:     name,
			target:   target,
			desc:     oneLineDesc(d.Description),
			schema:   normalizeSchema(d.InputSchema),
			readOnly: readOnly,
			conn:     conn,
		})
		sum.Names = append(sum.Names, name)
		if readOnly {
			sum.ReadOnlyNames = append(sum.ReadOnlyNames, name)
		}
		sum.Servers[serverLabel(name)]++
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

func registrationNames(advertised, namespace string) (name, target string, ok bool) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		if !validName(advertised) {
			return "", "", false
		}
		return advertised, advertised, true
	}
	qualified := namePrefix + namespace + "__" + advertised
	if !validName(qualified) {
		return "", "", false
	}
	return qualified, advertised, true
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

// ServerLabel is the exported form of serverLabel: it extracts the best-effort
// display-only server label from a validated mcp__<server>__<tool> name. It is
// used for display and for config-level per-server filtering, never for routing.
func ServerLabel(name string) string {
	return serverLabel(name)
}
