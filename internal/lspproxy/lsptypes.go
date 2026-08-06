package lspproxy

import "encoding/json"

// This file holds the minimal subset of LSP wire types the shim needs. Fields
// the shim does not use are omitted; decoding is tolerant of extras.

// Position is a zero-based line and UTF-16 character offset within a line.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range within a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// LocationLink is the link-style result some servers return for definition when
// the client advertises linkSupport.
type LocationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// WorkspaceFolder is a root folder advertised to the server.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// ClientInfo identifies this shim to the server.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeParams is the subset of the LSP initialize request the shim sends.
type InitializeParams struct {
	ProcessID             int               `json:"processId"`
	RootURI               string            `json:"rootUri"`
	WorkspaceFolders      []WorkspaceFolder `json:"workspaceFolders"`
	Capabilities          json.RawMessage   `json:"capabilities"`
	ClientInfo            ClientInfo        `json:"clientInfo"`
	InitializationOptions json.RawMessage   `json:"initializationOptions,omitempty"`
}

// TextDocumentIdentifier references a document by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier references a specific version of a document.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// TextDocumentItem is a full document sent on didOpen.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// DidOpenParams is the textDocument/didOpen notification payload.
type DidOpenParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// TextDocumentContentChangeEvent is a single change. The shim uses full-document
// sync only, so Text is the entire new content.
type TextDocumentContentChangeEvent struct {
	Text string `json:"text"`
}

// DidChangeParams is the textDocument/didChange notification payload.
type DidChangeParams struct {
	TextDocument   VersionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []TextDocumentContentChangeEvent `json:"contentChanges"`
}

// TextDocumentPositionParams locates a position within a document, the shape of
// definition/references/hover/rename requests.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// ReferenceContext configures a references request.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// ReferenceParams is the textDocument/references request payload.
type ReferenceParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      ReferenceContext       `json:"context"`
}

// DocumentSymbolParams is the textDocument/documentSymbol request payload.
type DocumentSymbolParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// WorkspaceSymbolParams is the workspace/symbol request payload.
type WorkspaceSymbolParams struct {
	Query string `json:"query"`
}

// CompletionParams is the textDocument/completion request payload. The context
// is omitted for ordinary invoked completion, which is accepted by LSP servers.
type CompletionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// CompletionItem is the subset of a completion candidate useful to an agent.
type CompletionItem struct {
	Label         string          `json:"label"`
	Kind          int             `json:"kind"`
	Detail        string          `json:"detail"`
	Documentation json.RawMessage `json:"documentation"`
	InsertText    string          `json:"insertText"`
	FilterText    string          `json:"filterText"`
}

// SignatureHelp is the textDocument/signatureHelp result.
type SignatureHelp struct {
	Signatures      []SignatureInformation `json:"signatures"`
	ActiveSignature int                    `json:"activeSignature"`
	ActiveParameter int                    `json:"activeParameter"`
}

// SignatureInformation describes one callable signature.
type SignatureInformation struct {
	Label           string                 `json:"label"`
	Documentation   json.RawMessage        `json:"documentation"`
	Parameters      []ParameterInformation `json:"parameters"`
	ActiveParameter *int                   `json:"activeParameter,omitempty"`
}

// ParameterInformation describes one parameter. Label is string or a pair of
// offsets in LSP, so it remains raw and is formatted defensively.
type ParameterInformation struct {
	Label         json.RawMessage `json:"label"`
	Documentation json.RawMessage `json:"documentation"`
}

// DocumentHighlight marks a textual/read/write occurrence in one document.
type DocumentHighlight struct {
	Range Range `json:"range"`
	Kind  int   `json:"kind"`
}

// CallHierarchyItem identifies a callable returned by prepareCallHierarchy.
type CallHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Detail         string          `json:"detail"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// CallHierarchyIncomingCall is one caller plus the ranges containing calls.
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCall is one callee plus call-site ranges in the source.
type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// TypeHierarchyItem identifies a type returned by prepareTypeHierarchy.
type TypeHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Detail         string          `json:"detail"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// InlayHint is a server-supplied inline type or parameter hint. Label may be a
// string or an array of label parts, so it remains raw until formatting.
type InlayHint struct {
	Position Position        `json:"position"`
	Label    json.RawMessage `json:"label"`
	Kind     int             `json:"kind"`
	Tooltip  json.RawMessage `json:"tooltip"`
}

// CodeActionContext and CodeActionParams request actions for a document range.
type CodeActionContext struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	Only        []string     `json:"only,omitempty"`
}

type CodeActionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Range        Range                  `json:"range"`
	Context      CodeActionContext      `json:"context"`
}

// CodeAction is the actionable subset of an LSP CodeAction. Edit stays raw so
// file operations can be rejected before applying. A direct Command response is
// normalized separately and marked CommandOnly.
type CodeAction struct {
	Title       string          `json:"title"`
	Kind        string          `json:"kind"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
	Edit        json.RawMessage `json:"edit"`
	Command     json.RawMessage `json:"command"`
	Data        json.RawMessage `json:"data"`
	Disabled    *struct {
		Reason string `json:"reason"`
	} `json:"disabled,omitempty"`
	CommandOnly bool `json:"-"`
}

// DocumentRangeParams is shared by range-based inspection requests.
type DocumentRangeParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Range        Range                  `json:"range"`
}

// FormattingOptions and document/range params drive server formatting.
type FormattingOptions struct {
	TabSize      int  `json:"tabSize"`
	InsertSpaces bool `json:"insertSpaces"`
}

type DocumentFormattingParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Options      FormattingOptions      `json:"options"`
}

type DocumentRangeFormattingParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Range        Range                  `json:"range"`
	Options      FormattingOptions      `json:"options"`
}

// DocumentSymbol is the hierarchical document-symbol shape.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children"`
}

// SymbolInformation is the flat document/workspace-symbol shape.
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName"`
}

// Symbol is the shim's normalized symbol, unifying the hierarchical and flat
// LSP shapes. Line is the zero-based line of the symbol; URI is set for flat
// (workspace) symbols and empty for in-file document symbols.
type Symbol struct {
	Name     string
	Detail   string
	Kind     int
	Line     int
	URI      string
	Children []Symbol
}

// TextEdit is a single replacement within a document.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// TextDocumentEdit is the documentChanges form of an edit set for one document.
type TextDocumentEdit struct {
	TextDocument VersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []TextEdit                      `json:"edits"`
}

// WorkspaceEdit is the result of textDocument/rename: edits keyed either by URI
// (changes) or as an ordered documentChanges list.
type WorkspaceEdit struct {
	Changes         map[string][]TextEdit `json:"changes"`
	DocumentChanges []TextDocumentEdit    `json:"documentChanges"`
}

// RenameParams is the textDocument/rename request payload.
type RenameParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	NewName      string                 `json:"newName"`
}

// FileEdits is the shim's normalized per-file edit set from a WorkspaceEdit.
type FileEdits struct {
	URI   string
	Edits []TextEdit
}

// Diagnostic is a single problem reported for a document. Code is string|number
// in LSP, kept raw and rendered by the formatter.
type Diagnostic struct {
	Range    Range           `json:"range"`
	Severity int             `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  string          `json:"message"`
}

// PublishDiagnosticsParams is the server-pushed textDocument/publishDiagnostics
// payload. Version, when present, identifies the document version the
// diagnostics correspond to.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// ServerCapabilities is the subset of the server's advertised capabilities the
// shim inspects. positionEncoding (LSP 3.17) tells the shim how to measure
// columns; absent means the default, UTF-16.
type ServerCapabilities struct {
	PositionEncoding string `json:"positionEncoding,omitempty"`
}

// ServerInfo identifies the language server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   *ServerInfo        `json:"serverInfo,omitempty"`
}
