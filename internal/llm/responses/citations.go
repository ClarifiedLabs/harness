package responses

import "harness/internal/llm"

// citationAssembler collects source attribution independently of text assembly:
// annotation snapshots often arrive after the corresponding text was streamed.
// The terminal provider event carries typed sources so the agent can choose a
// safe continuation boundary, then append ordinary assistant text that survives
// the transcript, recorder, and replay without a provider-specific UI event.
type citationAssembler struct {
	sources []llm.URLCitation
	seen    map[string]bool
	emitted bool
}

func (a *citationAssembler) add(citation wireURLCitation) {
	if citation.Type != "url_citation" || citation.URL == "" || a.seen[citation.URL] {
		return
	}
	if a.seen == nil {
		a.seen = make(map[string]bool)
	}
	a.seen[citation.URL] = true
	a.sources = append(a.sources, llm.URLCitation{URL: citation.URL, Title: citation.Title})
}

func (a *citationAssembler) part(part *wireContentPart) {
	if part == nil || part.Type != "output_text" {
		return
	}
	for _, citation := range part.Annotations {
		a.add(citation)
	}
}

func (a *citationAssembler) item(item *wireOutputItem) {
	if item == nil || item.Type != "message" {
		return
	}
	for i := range item.Content {
		a.part(&item.Content[i])
	}
}

func (a *citationAssembler) collect(event wireEvent) {
	switch event.Type {
	case "response.output_text.annotation.added", "response.output_text.annotation.done":
		if event.Annotation != nil {
			a.add(*event.Annotation)
		}
	case "response.content_part.done":
		a.part(event.Part)
	case "response.output_item.done":
		a.item(event.Item)
	case "response.completed", "response.incomplete":
		if event.Response != nil {
			for i := range event.Response.Output {
				a.item(&event.Response.Output[i])
			}
		}
	}
}

func (a *citationAssembler) take() []llm.URLCitation {
	if a.emitted {
		return nil
	}
	a.emitted = true
	return append([]llm.URLCitation(nil), a.sources...)
}
