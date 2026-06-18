package responses

import (
	"strings"

	"harness/internal/llm"
)

type outputKey struct {
	itemID       string
	outputIndex  int
	contentIndex int
	kind         string
}

type summaryKey struct {
	itemID       string
	outputIndex  int
	summaryIndex int
}

type phaseKey struct {
	itemID      string
	outputIndex int
}

type textAssembler struct {
	seen map[outputKey]bool
}

func newTextAssembler() *textAssembler {
	return &textAssembler{seen: map[outputKey]bool{}}
}

func (a *textAssembler) textDelta(event wireEvent, yield func(llm.StreamEvent, error) bool) bool {
	return a.emitDelta(a.key(event, "output_text"), event.Delta, yield)
}

func (a *textAssembler) textDone(event wireEvent, yield func(llm.StreamEvent, error) bool) bool {
	return a.emitText(a.key(event, "output_text"), event.Text, yield)
}

func (a *textAssembler) refusalDelta(event wireEvent, yield func(llm.StreamEvent, error) bool) bool {
	return a.emitDelta(a.key(event, "refusal"), event.Delta, yield)
}

func (a *textAssembler) refusalDone(event wireEvent, yield func(llm.StreamEvent, error) bool) bool {
	return a.emitText(a.key(event, "refusal"), event.Refusal, yield)
}

func (a *textAssembler) contentPartDone(event wireEvent, yield func(llm.StreamEvent, error) bool) bool {
	if event.Part == nil {
		return true
	}
	text, ok := contentPartText(*event.Part)
	if !ok {
		return true
	}
	return a.emitText(a.key(event, event.Part.Type), text, yield)
}

func (a *textAssembler) outputItem(index int, item *wireOutputItem, yield func(llm.StreamEvent, error) bool) bool {
	if item == nil || item.Type != "message" {
		return true
	}
	for i, part := range item.Content {
		text, ok := contentPartText(part)
		if !ok {
			continue
		}
		key := outputKey{itemID: item.ID, outputIndex: index, contentIndex: i, kind: part.Type}
		if !a.emitText(key, text, yield) {
			return false
		}
	}
	return true
}

func (a *textAssembler) key(event wireEvent, kind string) outputKey {
	return outputKey{
		itemID:       event.ItemID,
		outputIndex:  event.OutputIndex,
		contentIndex: event.ContentIndex,
		kind:         kind,
	}
}

func (a *textAssembler) emitText(key outputKey, text string, yield func(llm.StreamEvent, error) bool) bool {
	if text == "" || a.seen[key] {
		return true
	}
	a.seen[key] = true
	return yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: text}, nil)
}

func (a *textAssembler) emitDelta(key outputKey, text string, yield func(llm.StreamEvent, error) bool) bool {
	if text == "" {
		return true
	}
	a.seen[key] = true
	return yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: text}, nil)
}

func contentPartText(part wireContentPart) (string, bool) {
	switch part.Type {
	case "output_text":
		return part.Text, part.Text != ""
	case "refusal":
		if part.Refusal != "" {
			return part.Refusal, true
		}
		return part.Text, part.Text != ""
	default:
		return "", false
	}
}

type reasoningAssembler struct {
	buffers map[summaryKey]*strings.Builder
	emitted map[summaryKey]string
}

func newReasoningAssembler() *reasoningAssembler {
	return &reasoningAssembler{
		buffers: map[summaryKey]*strings.Builder{},
		emitted: map[summaryKey]string{},
	}
}

func (a *reasoningAssembler) summaryDelta(event wireEvent) bool {
	if event.Delta == "" {
		return true
	}
	a.buffer(a.key(event)).WriteString(event.Delta)
	return true
}

func (a *reasoningAssembler) summaryDone(event wireEvent, yield func(llm.StreamEvent, error) bool) bool {
	key := a.key(event)
	text := event.Text
	if text == "" {
		text = a.bufferText(key)
	}
	return a.emitComplete(key, text, yield)
}

func (a *reasoningAssembler) summaryPartDone(event wireEvent, yield func(llm.StreamEvent, error) bool) bool {
	if event.Part == nil || event.Part.Type != "summary_text" {
		return true
	}
	return a.emitComplete(a.key(event), event.Part.Text, yield)
}

func (a *reasoningAssembler) outputItem(index int, item *wireOutputItem, yield func(llm.StreamEvent, error) bool) bool {
	if item == nil || item.Type != "reasoning" {
		return true
	}
	for i, part := range item.Summary {
		if part.Type != "summary_text" || part.Text == "" {
			continue
		}
		key := summaryKey{itemID: item.ID, outputIndex: index, summaryIndex: i}
		if !a.emitComplete(key, part.Text, yield) {
			return false
		}
	}
	return true
}

func (a *reasoningAssembler) key(event wireEvent) summaryKey {
	return summaryKey{
		itemID:       event.ItemID,
		outputIndex:  event.OutputIndex,
		summaryIndex: event.SummaryIndex,
	}
}

func (a *reasoningAssembler) buffer(key summaryKey) *strings.Builder {
	b := a.buffers[key]
	if b == nil {
		b = &strings.Builder{}
		a.buffers[key] = b
	}
	return b
}

func (a *reasoningAssembler) bufferText(key summaryKey) string {
	if b := a.buffers[key]; b != nil {
		return b.String()
	}
	return ""
}

func (a *reasoningAssembler) emitComplete(key summaryKey, text string, yield func(llm.StreamEvent, error) bool) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}
	if a.emitted[key] != "" {
		return true
	}
	a.emitted[key] = text
	return yield(llm.StreamEvent{Kind: llm.EventReasoningSummary, Text: text}, nil)
}

func emitResponseOutputWithPhase(output []wireOutputItem, text *textAssembler, reasoning *reasoningAssembler, phase *phaseAssembler, yield func(llm.StreamEvent, error) bool) bool {
	for i := range output {
		item := &output[i]
		switch item.Type {
		case "message":
			if phase != nil && !phase.outputItem(i, item, yield) {
				return false
			}
			if !text.outputItem(i, item, yield) {
				return false
			}
		case "reasoning":
			if !reasoning.outputItem(i, item, yield) {
				return false
			}
		}
	}
	return true
}

type phaseAssembler struct {
	emitted map[phaseKey]string
}

func newPhaseAssembler() *phaseAssembler {
	return &phaseAssembler{emitted: map[phaseKey]string{}}
}

func (a *phaseAssembler) outputItem(index int, item *wireOutputItem, yield func(llm.StreamEvent, error) bool) bool {
	if item == nil || item.Type != "message" || item.Phase == "" {
		return true
	}
	key := phaseKey{itemID: item.ID, outputIndex: index}
	if a.emitted[key] == item.Phase {
		return true
	}
	a.emitted[key] = item.Phase
	return yield(llm.StreamEvent{Kind: llm.EventAssistantPhase, Phase: item.Phase}, nil)
}
