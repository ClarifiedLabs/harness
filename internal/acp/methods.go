package acp

// JSON-RPC method names in the stable ACP v1 surface implemented here. Update
// and cancel are notifications; all other names identify requests.
const (
	MethodInitialize               = "initialize"
	MethodSessionNew               = "session/new"
	MethodSessionPrompt            = "session/prompt"
	MethodSessionUpdate            = "session/update"
	MethodSessionCancel            = "session/cancel"
	MethodSessionClose             = "session/close"
	MethodSessionRequestPermission = "session/request_permission"
)
