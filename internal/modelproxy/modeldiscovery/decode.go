package modeldiscovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"harness/internal/llm"
	"harness/internal/modelcatalog"
)

func fetchPages(ctx context.Context, client *http.Client, spec Spec, headers http.Header, etag, codexClientVersion string) (map[string]Model, string, bool, error) {
	endpoint, err := url.Parse(spec.Endpoint)
	if err != nil {
		return nil, "", false, err
	}
	q := endpoint.Query()
	switch spec.Format {
	case FormatCodex:
		if strings.TrimSpace(codexClientVersion) != "" {
			q.Set("client_version", codexClientVersion)
		}
	case FormatAnthropic:
		q.Set("limit", "1000")
	case FormatGemini:
		q.Set("pageSize", "1000")
	}
	endpoint.RawQuery = q.Encode()
	models := map[string]Model{}
	responseETag := ""
	for page := 0; page < maxPages; page++ {
		body, next, gotETag, notModified, err := fetchPage(ctx, client, endpoint.String(), headers, etag, page == 0, spec)
		if err != nil {
			return nil, "", false, err
		}
		if notModified {
			return nil, "", true, nil
		}
		if gotETag != "" && page == 0 {
			responseETag = gotETag
		}
		for id, model := range body {
			models[id] = model
			if len(models) > maxModels {
				return nil, "", false, fmt.Errorf("provider model discovery exceeded %d models", maxModels)
			}
		}
		if next == "" {
			return models, responseETag, false, nil
		}
		nextURL, err := resolveNext(endpoint, next, spec.Format)
		if err != nil {
			return nil, "", false, err
		}
		if nextURL.String() == endpoint.String() {
			return nil, "", false, fmt.Errorf("provider model discovery pagination loop at %s", endpoint.Redacted())
		}
		endpoint = nextURL
		etag = ""
	}
	return nil, "", false, fmt.Errorf("provider model discovery exceeded %d pages", maxPages)
}

func fetchPage(ctx context.Context, client *http.Client, endpoint string, headers http.Header, etag string, first bool, spec Spec) (map[string]Model, string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", "", false, err
	}
	req.Header = headers.Clone()
	req.Header.Set("accept", "application/json")
	if first && etag != "" {
		req.Header.Set("if-none-match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && first {
		return nil, "", resp.Header.Get("etag"), true, nil
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		if spec.AutoEndpoint {
			return nil, "", "", false, fmt.Errorf("%w: GET %s: %s", ErrUnsupported, req.URL.Redacted(), resp.Status)
		}
		return nil, "", "", false, fmt.Errorf("provider model discovery: GET %s: %s", req.URL.Redacted(), resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", false, fmt.Errorf("provider model discovery: GET %s: %s", req.URL.Redacted(), resp.Status)
	}
	limited := io.LimitReader(resp.Body, maxResponseBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", "", false, err
	}
	if len(data) > maxResponseBody {
		return nil, "", "", false, fmt.Errorf("provider model discovery response exceeded %d bytes", maxResponseBody)
	}
	models, next, err := decodePage(data, spec)
	return models, next, resp.Header.Get("etag"), false, err
}

func resolveNext(current *url.URL, next string, format Format) (*url.URL, error) {
	if format == FormatOpenAI || format == FormatAnthropic {
		q := current.Query()
		key := "after"
		if format == FormatAnthropic {
			key = "after_id"
		}
		q.Set(key, next)
		currentCopy := *current
		currentCopy.RawQuery = q.Encode()
		return &currentCopy, nil
	}
	if format == FormatGemini {
		q := current.Query()
		q.Set("pageToken", next)
		currentCopy := *current
		currentCopy.RawQuery = q.Encode()
		return &currentCopy, nil
	}
	nextURL, err := url.Parse(next)
	if err != nil {
		return nil, fmt.Errorf("provider model discovery invalid next page: %w", err)
	}
	resolved := current.ResolveReference(nextURL)
	if !strings.EqualFold(resolved.Scheme, current.Scheme) || !strings.EqualFold(resolved.Host, current.Host) {
		return nil, fmt.Errorf("provider model discovery next page changed origin from %s to %s", current.Redacted(), resolved.Redacted())
	}
	return resolved, nil
}

func decodePage(data []byte, spec Spec) (map[string]Model, string, error) {
	switch spec.Format {
	case FormatOpenAI:
		return decodeOpenAI(data, spec)
	case FormatOpenRouter:
		return decodeOpenRouter(data, spec)
	case FormatCodex:
		return decodeCodex(data, spec)
	case FormatAnthropic:
		return decodeAnthropic(data, spec)
	case FormatGemini:
		return decodeGemini(data, spec)
	default:
		return nil, "", fmt.Errorf("unsupported model discovery format %q", spec.Format)
	}
}

type listEnvelope struct {
	Data    []json.RawMessage `json:"data"`
	Models  []json.RawMessage `json:"models"`
	HasMore bool              `json:"has_more"`
	LastID  string            `json:"last_id"`
	FirstID string            `json:"first_id"`
	Next    string            `json:"next"`
	Meta    struct {
		NextPage string `json:"next_page"`
	} `json:"meta"`
	NextPageToken string `json:"nextPageToken"`
}

func decodeOpenAI(data []byte, spec Spec) (map[string]Model, string, error) {
	var envelope listEnvelope
	if err := decodeOne(data, &envelope); err != nil {
		return nil, "", err
	}
	out := map[string]Model{}
	for _, raw := range envelope.Data {
		model, ok, err := decodeOpenAIModel(raw, spec)
		if err != nil {
			return nil, "", err
		}
		if ok {
			out[model.ID] = model
		}
	}
	next := ""
	if envelope.HasMore {
		next = strings.TrimSpace(envelope.LastID)
		if next == "" && len(envelope.Data) > 0 {
			var last struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(envelope.Data[len(envelope.Data)-1], &last)
			next = strings.TrimSpace(last.ID)
		}
		if next == "" {
			return nil, "", errors.New("provider model discovery response has_more without a cursor")
		}
	}
	return out, next, nil
}

func decodeOpenAIModel(raw json.RawMessage, spec Spec) (Model, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Model{}, false, err
	}
	id := rawString(fields, "id")
	if id == "" {
		return Model{}, false, nil
	}
	model := Model{ID: id, Name: rawString(fields, "name", "display_name")}
	model.ContextWindow = rawPositiveInt(fields, "context_length", "context_window", "max_context_length", "max_model_len")
	model.OutputLimit = rawPositiveInt(fields, "max_output_tokens", "max_completion_tokens", "output_limit")
	if modalities, ok := rawStringSlice(fields, "input_modalities"); ok {
		model.InputModalities, model.InputModalitiesKnown = normalizeModalities(modalities), true
	}
	if value, ok := rawBool(fields, "supports_reasoning", "reasoning"); ok {
		model.Reasoning = &value
	}
	if value, ok := rawBool(fields, "supports_reasoning_summary", "reasoning_summary_supported"); ok {
		model.ReasoningSummarySupported = &value
	}
	if efforts, ok := rawStringSlice(fields, "think_efforts", "reasoning_efforts"); ok && len(efforts) > 0 {
		model.ReasoningOptions = []llm.ReasoningOption{{Type: "effort", Values: efforts}}
		value := true
		model.Reasoning = &value
	}
	generative := model.ContextWindow != nil || model.OutputLimit != nil || model.InputModalitiesKnown || model.Reasoning != nil
	if capabilities, ok := fields["capabilities"]; ok {
		var caps map[string]json.RawMessage
		if json.Unmarshal(capabilities, &caps) == nil {
			for _, key := range []string{"chat_completion", "completion", "responses", "generate", "text_generation"} {
				if value, found := rawBool(caps, key); found && value {
					generative = true
				}
			}
		}
	}
	model.Eligible = spec.IncludeUnknownModels || spec.TrustedGenerative || generative
	return model, true, nil
}

func decodeOpenRouter(data []byte, spec Spec) (map[string]Model, string, error) {
	var envelope struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Architecture  struct {
				InputModalities  []string `json:"input_modalities"`
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
			TopProvider struct {
				ContextLength       int `json:"context_length"`
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
			Reasoning           *struct {
				SupportedEfforts []string `json:"supported_efforts"`
			} `json:"reasoning"`
			Pricing json.RawMessage `json:"pricing"`
		} `json:"data"`
		Next string `json:"next"`
		Meta struct {
			NextPage string `json:"next_page"`
		} `json:"meta"`
	}
	if err := decodeOne(data, &envelope); err != nil {
		return nil, "", err
	}
	out := map[string]Model{}
	for _, item := range envelope.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		outputText := slices.ContainsFunc(item.Architecture.OutputModalities, func(v string) bool { return strings.EqualFold(v, "text") })
		if !outputText && !spec.IncludeUnknownModels {
			continue
		}
		model := Model{ID: id, Name: item.Name, Eligible: outputText || spec.IncludeUnknownModels}
		contextWindow := item.ContextLength
		if contextWindow <= 0 {
			contextWindow = item.TopProvider.ContextLength
		}
		if contextWindow > 0 {
			model.ContextWindow = &contextWindow
		}
		if item.TopProvider.MaxCompletionTokens > 0 {
			value := item.TopProvider.MaxCompletionTokens
			model.OutputLimit = &value
		}
		if item.Architecture.InputModalities != nil {
			model.InputModalities = normalizeModalities(item.Architecture.InputModalities)
			model.InputModalitiesKnown = true
		}
		reasoning := item.Reasoning != nil || slices.ContainsFunc(item.SupportedParameters, func(v string) bool {
			return strings.EqualFold(v, "reasoning") || strings.EqualFold(v, "reasoning_effort")
		})
		model.Reasoning = &reasoning
		if item.Reasoning != nil && len(item.Reasoning.SupportedEfforts) > 0 {
			model.ReasoningOptions = []llm.ReasoningOption{{Type: "effort", Values: item.Reasoning.SupportedEfforts}}
		}
		if price, ok := openRouterPrice(item.Pricing); ok {
			model.Price = &price
		}
		out[id] = model
	}
	next := strings.TrimSpace(envelope.Next)
	if next == "" {
		next = strings.TrimSpace(envelope.Meta.NextPage)
	}
	return out, next, nil
}

func openRouterPrice(raw json.RawMessage) (llm.Price, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return llm.Price{}, false
	}
	var price llm.Price
	if !applyOpenRouterRates(fields, &price) {
		return llm.Price{}, false
	}
	price.Tiers = openRouterPriceTiers(fields, price)
	return price, true
}

type openRouterPriceOverride struct {
	threshold int
	fields    map[string]json.RawMessage
}

func openRouterPriceTiers(fields map[string]json.RawMessage, base llm.Price) []llm.PriceTier {
	var items []json.RawMessage
	if json.Unmarshal(fields["overrides"], &items) != nil {
		return nil
	}
	overrides := make([]openRouterPriceOverride, 0, len(items))
	thresholdSet := make(map[int]struct{}, len(items))
	for _, raw := range items {
		var overrideFields map[string]json.RawMessage
		if json.Unmarshal(raw, &overrideFields) != nil || !supportedOpenRouterOverride(overrideFields) {
			continue
		}
		var threshold int
		if json.Unmarshal(overrideFields["min_prompt_tokens"], &threshold) != nil || threshold < 0 {
			continue
		}
		candidate := base
		if !applyOpenRouterRates(overrideFields, &candidate) {
			continue
		}
		overrides = append(overrides, openRouterPriceOverride{threshold: threshold, fields: overrideFields})
		thresholdSet[threshold] = struct{}{}
	}

	thresholds := make([]int, 0, len(thresholdSet))
	for threshold := range thresholdSet {
		thresholds = append(thresholds, threshold)
	}
	slices.Sort(thresholds)
	tiers := make([]llm.PriceTier, 0, len(thresholds))
	for _, threshold := range thresholds {
		effective := base
		for _, override := range overrides {
			if override.threshold <= threshold {
				applyOpenRouterRates(override.fields, &effective)
			}
		}
		tiers = append(tiers, openRouterPriceTier(threshold, effective))
	}
	return tiers
}

func supportedOpenRouterOverride(fields map[string]json.RawMessage) bool {
	for key := range fields {
		switch key {
		case "min_prompt_tokens",
			"prompt", "completion",
			"input_cache_read", "input_cache_write", "input_cache_write_1h",
			"internal_reasoning", "audio", "audio_output", "input_audio", "output_audio",
			"image", "image_output", "input_audio_cache", "web_search", "request", "discount":
		default:
			return false
		}
	}
	return true
}

func applyOpenRouterRates(fields map[string]json.RawMessage, price *llm.Price) bool {
	known := false
	for _, field := range []struct {
		key    string
		target *float64
	}{
		{key: "prompt", target: &price.Input},
		{key: "completion", target: &price.Output},
		{key: "input_cache_read", target: &price.CacheRead},
		{key: "input_cache_write", target: &price.CacheWrite},
		{key: "input_cache_write_1h", target: &price.CacheWrite1h},
		{key: "internal_reasoning", target: &price.Reasoning},
		{key: "input_audio", target: &price.InputAudio},
		{key: "audio", target: &price.InputAudio},
		{key: "output_audio", target: &price.OutputAudio},
		{key: "audio_output", target: &price.OutputAudio},
	} {
		if value, ok := openRouterRate(fields, field.key); ok {
			*field.target = value
			known = true
		}
	}
	return known
}

func openRouterRate(fields map[string]json.RawMessage, key string) (float64, bool) {
	var raw string
	if json.Unmarshal(fields[key], &raw) != nil || strings.TrimSpace(raw) == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	value *= 1_000_000
	if math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func openRouterPriceTier(threshold int, price llm.Price) llm.PriceTier {
	return llm.PriceTier{
		Threshold: threshold, Input: price.Input, Output: price.Output,
		CacheRead: price.CacheRead, CacheWrite: price.CacheWrite, CacheWrite1h: price.CacheWrite1h,
		Reasoning: price.Reasoning, InputAudio: price.InputAudio, OutputAudio: price.OutputAudio,
	}
}

func decodeCodex(data []byte, spec Spec) (map[string]Model, string, error) {
	var envelope struct {
		Models []struct {
			Slug             string   `json:"slug"`
			DisplayName      string   `json:"display_name"`
			ContextWindow    int      `json:"context_window"`
			MaxContextWindow int      `json:"max_context_window"`
			InputModalities  []string `json:"input_modalities"`
			Visibility       string   `json:"visibility"`
			ReasoningLevels  []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			SummaryParameter   *bool `json:"supports_reasoning_summary_parameter"`
			ReasoningSummaries *bool `json:"supports_reasoning_summaries"`
			ServiceTiers       []struct {
				ID, Name, Description string
			} `json:"service_tiers"`
			AdditionalSpeedTiers []string `json:"additional_speed_tiers"`
		} `json:"models"`
	}
	if err := decodeOne(data, &envelope); err != nil {
		return nil, "", err
	}
	out := map[string]Model{}
	for _, item := range envelope.Models {
		id := strings.TrimSpace(item.Slug)
		visibility := strings.ToLower(strings.TrimSpace(item.Visibility))
		if id == "" || (visibility != "" && visibility != "list") {
			continue
		}
		model := Model{ID: id, Name: item.DisplayName, Eligible: true}
		contextWindow := item.ContextWindow
		if contextWindow <= 0 {
			contextWindow = item.MaxContextWindow
		}
		if contextWindow > 0 {
			model.ContextWindow = &contextWindow
		}
		if item.InputModalities != nil {
			model.InputModalities = normalizeModalities(item.InputModalities)
			model.InputModalitiesKnown = true
		}
		efforts := make([]string, 0, len(item.ReasoningLevels))
		for _, level := range item.ReasoningLevels {
			if effort := strings.TrimSpace(level.Effort); effort != "" {
				efforts = append(efforts, effort)
			}
		}
		reasoning := len(efforts) > 0
		model.Reasoning = &reasoning
		if reasoning {
			model.ReasoningOptions = []llm.ReasoningOption{{Type: "effort", Values: efforts}}
		}
		model.ReasoningSummarySupported = item.SummaryParameter
		if model.ReasoningSummarySupported == nil {
			model.ReasoningSummarySupported = item.ReasoningSummaries
		}
		for _, tier := range item.ServiceTiers {
			model.ServiceTiers = append(model.ServiceTiers, llm.ServiceTier{ID: tier.ID, Name: tier.Name, Description: tier.Description, Request: llm.ServiceTierRequest{ServiceTier: tier.ID}})
		}
		for _, speed := range item.AdditionalSpeedTiers {
			speed = strings.ToLower(strings.TrimSpace(speed))
			if speed == "" {
				continue
			}
			name := speed
			if speed == "fast" {
				name = "Fast"
			}
			model.ServiceTiers = append(model.ServiceTiers, llm.ServiceTier{ID: speed, Name: name, Request: llm.ServiceTierRequest{ServiceTier: speed}})
		}
		model.ServiceTiers = modelcatalog.NormalizeCodexFastServiceTiers(model.ServiceTiers)
		out[id] = model
	}
	return out, "", nil
}

func decodeAnthropic(data []byte, _ Spec) (map[string]Model, string, error) {
	var envelope struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := decodeOne(data, &envelope); err != nil {
		return nil, "", err
	}
	out := map[string]Model{}
	for _, item := range envelope.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			out[id] = Model{ID: id, Name: item.DisplayName, Eligible: true}
		}
	}
	next := ""
	if envelope.HasMore {
		next = strings.TrimSpace(envelope.LastID)
		if next == "" {
			return nil, "", errors.New("Anthropic model discovery response has_more without last_id")
		}
	}
	return out, next, nil
}

func decodeGemini(data []byte, _ Spec) (map[string]Model, string, error) {
	var envelope struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			InputTokenLimit            int      `json:"inputTokenLimit"`
			OutputTokenLimit           int      `json:"outputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			Thinking                   *bool    `json:"thinking"`
		} `json:"models"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := decodeOne(data, &envelope); err != nil {
		return nil, "", err
	}
	out := map[string]Model{}
	for _, item := range envelope.Models {
		if !slices.Contains(item.SupportedGenerationMethods, "generateContent") {
			continue
		}
		id := strings.TrimPrefix(strings.TrimSpace(item.Name), "models/")
		if id == "" {
			continue
		}
		model := Model{ID: id, Name: item.DisplayName, Eligible: true, Reasoning: item.Thinking}
		if item.InputTokenLimit > 0 {
			value := item.InputTokenLimit
			model.ContextWindow = &value
		}
		if item.OutputTokenLimit > 0 {
			value := item.OutputTokenLimit
			model.OutputLimit = &value
		}
		out[id] = model
	}
	return out, strings.TrimSpace(envelope.NextPageToken), nil
}

func decodeOne(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("provider model discovery response contains multiple JSON values")
		}
		return err
	}
	return nil
}

func rawString(fields map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func rawPositiveInt(fields map[string]json.RawMessage, keys ...string) *int {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			continue
		}
		value, err := strconv.Atoi(number.String())
		if err == nil && value > 0 {
			return &value
		}
	}
	return nil
}

func rawBool(fields map[string]json.RawMessage, keys ...string) (bool, bool) {
	for _, key := range keys {
		var value bool
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil {
			return value, true
		}
	}
	return false, false
}

func rawStringSlice(fields map[string]json.RawMessage, keys ...string) ([]string, bool) {
	for _, key := range keys {
		var values []string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &values) == nil {
			return values, true
		}
	}
	return nil, false
}

func normalizeModalities(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return out
}
