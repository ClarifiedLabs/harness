package ui

import (
	"bytes"
	"fmt"
	"testing"
)

type testModelPickerEntry struct {
	id    string
	name  string
	price string
}

func (m testModelPickerEntry) PickerID() string    { return m.id }
func (m testModelPickerEntry) PickerName() string  { return m.name }
func (m testModelPickerEntry) PickerPrice() string { return m.price }

func TestPrintModelPickerPageOmitsRedundantColumns(t *testing.T) {
	const longID = "openrouter:nvidia/a-model-name-longer-than-thirty-four-characters"
	models := []testModelPickerEntry{
		{id: "deepseek:deepseek-v4-flash", name: "deepseek-v4-flash", price: "$0.14/$0.28"},
		{id: longID, name: "Friendly model name"},
	}

	var out bytes.Buffer
	PrintModelPickerPage(&out, "targets", models, 0, 20, "")

	want := "Models for targets 1-2 of 2\n" +
		ModelPickerPriceLegend + "\n" +
		fmt.Sprintf("%4d. %-*s %s\n", 1, len(longID), models[0].id, models[0].price) +
		fmt.Sprintf("%4d. %-*s %s\n", 2, len(longID), models[1].id, "-")
	if got := out.String(); got != want {
		t.Fatalf("PrintModelPickerPage() output:\n%s\nwant:\n%s", got, want)
	}
}
