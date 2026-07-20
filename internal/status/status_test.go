package status

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		event string
		want  State
	}{
		{"started", Working},
		{"RUNNING", Working},
		{"completed", Attention},
		{"failed", Error},
		{"idle", Idle},
	}

	for _, test := range tests {
		got, err := Normalize(test.event)
		if err != nil {
			t.Fatalf("Normalize(%q) returned error: %v", test.event, err)
		}
		if got != test.want {
			t.Fatalf("Normalize(%q) = %q, want %q", test.event, got, test.want)
		}
	}
}

func TestNormalizeRejectsUnknownEvent(t *testing.T) {
	if _, err := Normalize("unknown"); err == nil {
		t.Fatal("Normalize(unknown) returned no error")
	}
}
