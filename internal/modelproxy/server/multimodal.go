package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"harness/internal/llm"
)

const (
	maxShapeDistinctValues = 16
	maxShapeDimension      = 1_000_000
	maxSanitizedErrorBytes = 2048
	maxSanitizerInputBytes = 16 * 1024
	maxSanitizerValues     = 256
	maxSanitizerImages     = 64
	maxSanitizerValueBytes = 4 * 1024
	maxSanitizerTotalBytes = 64 * 1024
	maxSanitizerJSONDepth  = 8

	genericImageErrorCode    = "image_request_failed"
	genericImageErrorMessage = "image-bearing model request failed; provider details were redacted"
)

var (
	imageDataURLPattern = regexp.MustCompile(`(?i)data:image/[a-z0-9.+-]+(?:;[a-z0-9._=+-]+)*;base64,[a-z0-9_+/=-]+`)
	longBase64Pattern   = regexp.MustCompile(`[A-Za-z0-9+/]{80,}={0,2}`)
)

// richToolResultShape derives a safe summary from the exact neutral request
// passed to a provider. It returns nil for text-only and manual-image requests:
// only images attached to tool results establish this compatibility shape.
func richToolResultShape(req llm.Request, apiType string) *llm.MultimodalRequestShape {
	shape := &llm.MultimodalRequestShape{Strategy: multimodalLoweringStrategy(apiType)}
	mimeTypes := map[string]struct{}{}
	details := map[string]struct{}{}
	dimensions := map[[2]int]int{}
	resultHash := sha256.New()
	imageHash := sha256.New()

	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Kind != llm.BlockToolResult {
				continue
			}
			shape.ToolResultCount++
			writeFingerprintValue(resultHash, []byte(block.ResultForID))
			for _, child := range block.ResultContent {
				if child.Kind != llm.BlockImage {
					continue
				}
				shape.ImageCount++
				shape.EncodedBytes = saturatingAddInt64(shape.EncodedBytes, int64(len(child.ImageData)))
				shape.DecodedBytes = saturatingAddInt64(shape.DecodedBytes, decodedImageSize(child.ImageData))
				writeFingerprintValue(imageHash, []byte(child.ImageData))
				mimeTypes[safeMIMEType(child.ImageMediaType)] = struct{}{}
				details[safeImageDetail(child.ImageDetail)] = struct{}{}
				width := boundedDimension(child.ImageWidth)
				height := boundedDimension(child.ImageHeight)
				if width > 0 || height > 0 {
					dimensions[[2]int{width, height}]++
				}
			}
		}
	}
	if shape.ImageCount == 0 {
		return nil
	}

	shape.MIMETypes = boundedSortedSet(mimeTypes)
	shape.Details = boundedSortedSet(details)
	shape.Dimensions = boundedSortedDimensions(dimensions)
	shape.ResultIDsSHA256 = hex.EncodeToString(resultHash.Sum(nil))
	shape.ImagePayloadsSHA256 = hex.EncodeToString(imageHash.Sum(nil))
	return shape
}

func multimodalLoweringStrategy(apiType string) string {
	switch strings.ToLower(strings.TrimSpace(apiType)) {
	case "anthropic":
		return llm.MultimodalStrategyAnthropicToolResultContent
	case "responses":
		return llm.MultimodalStrategyResponsesOutputThenImage
	default:
		return llm.MultimodalStrategyOpenAIToolThenUserImage
	}
}

func writeFingerprintValue(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func decodedImageSize(data string) int64 {
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		decoded, err := encoding.DecodeString(data)
		if err == nil {
			return int64(len(decoded))
		}
	}
	return 0
}

func saturatingAddInt64(total, value int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if value > 0 && total > maxInt64-value {
		return maxInt64
	}
	return total + value
}

func safeMIMEType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unspecified"
	}
	if len(value) > 64 || !strings.Contains(value, "/") {
		return "other"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("/+.-", r) {
			continue
		}
		return "other"
	}
	return value
}

func safeImageDetail(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "":
		return "unspecified"
	case "auto", "low", "high", "original":
		return value
	default:
		return "other"
	}
}

func boundedDimension(value int) int {
	switch {
	case value <= 0:
		return 0
	case value > maxShapeDimension:
		return maxShapeDimension
	default:
		return value
	}
}

func boundedSortedSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	if len(out) > maxShapeDistinctValues {
		out = out[:maxShapeDistinctValues]
	}
	return out
}

func boundedSortedDimensions(values map[[2]int]int) []llm.ImageDimension {
	keys := make([][2]int, 0, len(values))
	for dimensions := range values {
		keys = append(keys, dimensions)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] == keys[j][0] {
			return keys[i][1] < keys[j][1]
		}
		return keys[i][0] < keys[j][0]
	})
	if len(keys) > maxShapeDistinctValues {
		keys = keys[:maxShapeDistinctValues]
	}
	out := make([]llm.ImageDimension, 0, len(keys))
	for _, dimensions := range keys {
		out = append(out, llm.ImageDimension{
			Width:  dimensions[0],
			Height: dimensions[1],
			Count:  values[dimensions],
		})
	}
	return out
}

// classifyMultimodalToolResultRejection recognizes only final, non-retryable,
// targeted provider rejections. Generic invalid requests and unrelated provider
// failures intentionally remain unclassified.
func classifyMultimodalToolResultRejection(err error, shape *llm.MultimodalRequestShape) *llm.CompatibilityDiagnostic {
	if shape == nil || shape.ImageCount == 0 {
		return nil
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Retryable {
		return nil
	}
	if apiErr.StatusCode != 0 &&
		apiErr.StatusCode != http.StatusBadRequest &&
		apiErr.StatusCode != http.StatusRequestEntityTooLarge &&
		apiErr.StatusCode != http.StatusUnsupportedMediaType &&
		apiErr.StatusCode != http.StatusUnprocessableEntity {
		return nil
	}

	text := strings.ToLower(strings.TrimSpace(apiErr.Code + " " + apiErr.Message))
	if text == "" || containsAny(text,
		"unauthorized", "authentication", "invalid api key", "invalid_api_key", "permission denied",
		"rate limit", "rate_limit", "too many requests", "cancelled", "canceled",
		"server_error", "internal server", "temporarily unavailable", "overloaded", "timeout",
	) {
		return nil
	}

	imageSubject := containsAny(text,
		"image", "vision", "multimodal", "multi-modal", "input_image", "image_url",
	)
	toolResultSubject := containsAny(text,
		"tool result", "tool_result", "tool message", "role tool", `role "tool"`, `role 'tool'`,
		"function_call_output", "function output",
	)
	imageEncodingSubject := containsAny(text,
		"data url", "data-url", "data:", "base64", "media type", "media_type", "mime", "detail",
	)
	if !imageSubject && !toolResultSubject && !imageEncodingSubject {
		return nil
	}

	payloadLimit := containsAny(text,
		"payload too large", "request too large", "content too large", "image too large",
		"maximum image size", "max image size", "exceeds the", "size limit",
	)
	textOnly := containsAny(text,
		"text only", "text-only", "must be text", "only text", "expected string", "string content",
	)
	roleOrOrder := containsAny(text,
		"message order", "message ordering", "must follow", "must precede", "role", "adjacent",
	)
	invalid := containsAny(text,
		"unsupported", "not supported", "invalid", "malformed", "unrecognized", "unknown",
		"not allowed", "disallowed", "cannot", "can't", "failed to parse", "expected", "must be",
	)
	if !payloadLimit && !textOnly && !roleOrOrder && !invalid {
		return nil
	}

	reason := "image_content_rejected"
	switch {
	case payloadLimit:
		reason = "payload_limit"
	case textOnly:
		reason = "text_only_required"
	case imageEncodingSubject:
		reason = "invalid_image_encoding"
	case toolResultSubject:
		reason = "structured_tool_result_unsupported"
	case roleOrOrder:
		reason = "role_or_order_rejected"
	case containsAny(text, "unsupported", "not supported", "not allowed", "disallowed"):
		reason = "image_unsupported"
	}

	return &llm.CompatibilityDiagnostic{
		Category:    llm.CompatibilityCategoryMultimodalToolResultRejected,
		Reason:      reason,
		Confidence:  llm.CompatibilityConfidenceLikely,
		Remediation: "Use a target that accepts image-bearing tool results, or inspect the image outside this model call.",
		Strategy:    shape.Strategy,
	}
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func upstreamErrorStage(err error) llm.APIErrorStage {
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode != 0 {
		return llm.APIErrorStageUpstreamHTTP
	}
	return llm.APIErrorStageUpstreamStream
}

func upstreamRequestIDFromError(err error) string {
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) && apiErr.Diagnostic != nil {
		return llm.NormalizeUpstreamRequestID(apiErr.Diagnostic.UpstreamRequestID)
	}
	return ""
}

func withAPIErrorDiagnostic(err error, diagnostic *llm.APIErrorDiagnostic) error {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		return &llm.APIError{Message: err.Error(), Retryable: true, Diagnostic: diagnostic}
	}
	copyError := *apiErr
	copyError.Diagnostic = diagnostic
	return &copyError
}

// redactImageBearingError sanitizes provider text whenever the exact request
// contains any image, including ordinary manual image input. Classification is
// deliberately separate and remains limited to rich tool-result images.
func redactImageBearingError(err error, req llm.Request) error {
	if err == nil || !requestContainsImage(req) {
		return err
	}
	collected := collectRequestValues(req)
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		copyError := *apiErr
		// Provider payloads may echo image data inside otherwise legitimate
		// error-message fields, where generic field-name redaction cannot
		// distinguish it safely.
		copyError.ResponsePayload = ""
		if code, ok := sanitizeImageErrorMessage(apiErr.Code, collected); ok {
			copyError.Code = code
		} else {
			copyError.Code = genericImageErrorCode
		}
		if message, ok := sanitizeImageErrorMessage(apiErr.Message, collected); ok {
			copyError.Message = message
		} else {
			copyError.Message = genericImageErrorMessage
		}
		return &copyError
	}
	message, ok := sanitizeImageErrorMessage(err.Error(), collected)
	if !ok {
		message = genericImageErrorMessage
	}
	return errors.New(message)
}

func requestContainsImage(req llm.Request) bool {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockImage {
				return true
			}
			for _, child := range block.ResultContent {
				if child.Kind == llm.BlockImage {
					return true
				}
			}
		}
	}
	return false
}

type requestValues struct {
	values      []string
	imageValues []string
	seen        map[string]struct{}
	imageSeen   map[string]struct{}
	totalBytes  int
	complete    bool
}

func collectRequestValues(req llm.Request) requestValues {
	collected := requestValues{
		seen:      map[string]struct{}{},
		imageSeen: map[string]struct{}{},
		complete:  true,
	}
	collected.add(req.Model)
	collected.add(req.System)
	for _, value := range req.RequestContext {
		collected.add(value)
	}
	for _, value := range req.StopSeqs {
		collected.add(value)
	}
	collected.add(req.Reasoning.Profile)
	collected.add(req.Reasoning.Effort)
	collected.add(req.Reasoning.Summary)
	collected.add(req.ServiceTier)
	collected.add(req.Speed)
	for _, value := range req.Betas {
		collected.add(value)
	}
	collected.add(req.PreviousResponseID)
	collected.add(req.PromptCacheKey)
	for _, tool := range req.Tools {
		collected.add(tool.Name)
		collected.add(tool.Description)
		collected.addJSON(tool.Parameters, 0)
	}
	for _, group := range req.DeferredToolGroups {
		collected.add(group.Name)
		collected.add(group.Description)
		for _, tool := range group.Tools {
			collected.add(tool.Name)
			collected.add(tool.Description)
			collected.addJSON(tool.Parameters, 0)
		}
	}
	collected.add(req.ToolSearchFallback)
	for _, tool := range req.ServerTools {
		collected.add(tool.Name)
		collected.add(tool.Kind)
		collected.addJSON(tool.Parameters, 0)
	}
	for _, message := range req.Messages {
		collected.add(message.Phase)
		for _, block := range message.Content {
			collected.addBlock(block, 0)
		}
	}
	return collected
}

func (c *requestValues) add(value string) {
	if value == "" {
		return
	}
	if len(value) < 4 || len(value) > maxSanitizerValueBytes {
		c.complete = false
		return
	}
	if _, ok := c.seen[value]; ok {
		return
	}
	if len(c.values) >= maxSanitizerValues || len(value) > maxSanitizerTotalBytes-c.totalBytes {
		c.complete = false
		return
	}
	c.seen[value] = struct{}{}
	c.values = append(c.values, value)
	c.totalBytes += len(value)
}

func (c *requestValues) addImage(value string) {
	if value == "" {
		return
	}
	if _, ok := c.imageSeen[value]; ok {
		return
	}
	if len(value) > maxSanitizerInputBytes ||
		len(c.imageValues) >= maxSanitizerImages ||
		len(value) > maxSanitizerTotalBytes-c.totalBytes {
		c.complete = false
		return
	}
	c.imageSeen[value] = struct{}{}
	c.imageValues = append(c.imageValues, value)
	c.totalBytes += len(value)
}

func (c *requestValues) addBlock(block llm.ContentBlock, depth int) {
	if depth > maxSanitizerJSONDepth {
		c.complete = false
		return
	}
	c.add(block.Text)
	c.add(block.ImageMediaType)
	c.add(block.ImageDetail)
	c.add(block.ImageName)
	c.add(block.ToolUseID)
	c.add(block.ToolName)
	c.add(block.ToolNamespace)
	c.addJSON(block.ToolInput, depth)
	c.add(block.ResultForID)
	c.add(block.ResultText)
	c.add(block.Thinking)
	c.add(block.ThinkingSignature)
	c.add(block.RedactedData)
	c.add(block.ReasoningID)
	c.add(block.ReasoningEncrypted)
	c.addJSON(block.ResponsesToolSearch, depth)
	c.addJSON(block.AnthropicToolSearch, depth)
	if block.Kind == llm.BlockImage {
		c.addImage(block.ImageData)
		if len(block.ImageData)+len(block.ImageMediaType)+13 <= maxSanitizerInputBytes {
			c.addImage(llm.ImageDataURL(block))
		}
	}
	for _, child := range block.ResultContent {
		c.addBlock(child, depth+1)
	}
}

func (c *requestValues) addJSON(raw json.RawMessage, depth int) {
	if len(raw) == 0 {
		return
	}
	if len(raw) >= 4 {
		c.add(string(raw))
	}
	if depth > maxSanitizerJSONDepth {
		c.complete = false
		return
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		c.complete = false
		return
	}
	c.addJSONValue(value, depth)
}

func (c *requestValues) addJSONValue(value any, depth int) {
	if depth > maxSanitizerJSONDepth {
		c.complete = false
		return
	}
	switch typed := value.(type) {
	case string:
		c.add(typed)
		trimmed := strings.TrimSpace(typed)
		if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') {
			var embedded any
			if json.Unmarshal([]byte(trimmed), &embedded) == nil {
				c.addJSONValue(embedded, depth+1)
			}
		}
	case []any:
		for _, child := range typed {
			c.addJSONValue(child, depth+1)
		}
	case map[string]any:
		for key, child := range typed {
			c.add(key)
			c.addJSONValue(child, depth+1)
		}
	}
}

func sanitizeImageErrorMessage(message string, collected requestValues) (string, bool) {
	if message == "" {
		return "", collected.complete
	}
	if !collected.complete {
		return "", false
	}
	if len(message) > maxSanitizerInputBytes {
		message = message[:maxSanitizerInputBytes]
		for len(message) > 0 && !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	sort.Slice(collected.imageValues, func(i, j int) bool {
		return len(collected.imageValues[i]) > len(collected.imageValues[j])
	})
	for _, value := range collected.imageValues {
		if value != "" {
			message = strings.ReplaceAll(message, value, "[REDACTED_IMAGE_DATA]")
		}
	}
	message = imageDataURLPattern.ReplaceAllString(message, "[REDACTED_IMAGE_DATA_URL]")
	message = longBase64Pattern.ReplaceAllString(message, "[REDACTED_BASE64]")

	sort.Slice(collected.values, func(i, j int) bool {
		return len(collected.values[i]) > len(collected.values[j])
	})
	for _, value := range collected.values {
		message = strings.ReplaceAll(message, value, "[REDACTED_REQUEST_CONTENT]")
	}
	message = strings.TrimSpace(message)
	if len(message) > maxSanitizedErrorBytes {
		message = message[:maxSanitizedErrorBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += "…"
	}
	if !utf8.ValidString(message) || containsUnsafeControl(message) ||
		imageDataURLPattern.MatchString(message) || longBase64Pattern.MatchString(message) {
		return "", false
	}
	for _, value := range collected.values {
		if strings.Contains(message, value) {
			return "", false
		}
	}
	return message, true
}

func containsUnsafeControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func diagnosticLogAttrs(diagnostic *llm.APIErrorDiagnostic) []any {
	if diagnostic == nil {
		return nil
	}
	attrs := []any{
		"error_stage", string(diagnostic.Stage),
		"proxy_request_id", diagnostic.ProxyRequestID,
	}
	if diagnostic.UpstreamRequestID != "" {
		attrs = append(attrs, "upstream_request_id", diagnostic.UpstreamRequestID)
	}
	if compatibility := diagnostic.Compatibility; compatibility != nil {
		attrs = append(attrs,
			"diagnostic_kind", "compatibility_rejection",
			"category", compatibility.Category,
			"reason", compatibility.Reason,
			"confidence", compatibility.Confidence,
			"remediation", compatibility.Remediation,
			"strategy", compatibility.Strategy,
		)
	}
	return attrs
}

func shapeLogAttrs(shape *llm.MultimodalRequestShape) []any {
	if shape == nil {
		return nil
	}
	return []any{
		"strategy", shape.Strategy,
		"multimodal_shape", shape,
	}
}
