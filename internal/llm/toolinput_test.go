package llm

import "testing"

func TestNormalizeToolInputObject(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "object", input: `{"path":"x"}`, want: `{"path":"x"}`},
		{name: "empty", input: ``, want: `{}`},
		{name: "whitespace empty", input: " \n\t ", want: `{}`},
		{name: "trims object", input: " \n {\"path\":\"x\"}\t", want: `{"path":"x"}`},
		{name: "array", input: `[]`, wantErr: true},
		{name: "string", input: `"x"`, wantErr: true},
		{name: "null", input: `null`, wantErr: true},
		{name: "invalid", input: `{"path":`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeToolInputObject([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeToolInputObject(%q) succeeded with %s", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeToolInputObject(%q): %v", tt.input, err)
			}
			if string(got) != tt.want {
				t.Fatalf("NormalizeToolInputObject(%q) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateToolInputObjectRejectsEmpty(t *testing.T) {
	if err := ValidateToolInputObject(nil); err == nil {
		t.Fatal("ValidateToolInputObject(nil) = nil, want error")
	}
}
