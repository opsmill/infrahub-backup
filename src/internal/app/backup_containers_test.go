package app

import "testing"

func TestParsePrefectMaxLimit(t *testing.T) {
	tests := []struct {
		name      string
		parts     []string
		wantLimit int
		wantOK    bool
	}{
		{
			name:      "default cap in 422 detail",
			parts:     []string{"", "Response: {'detail': 'Invalid limit: must be less than or equal to 200.'}"},
			wantLimit: 200,
			wantOK:    true,
		},
		{
			name:      "non-default cap",
			parts:     []string{"['InfrahubTask'] Client error '422' ... must be less than or equal to 100."},
			wantLimit: 100,
			wantOK:    true,
		},
		{
			name:      "found in error text rather than output",
			parts:     []string{"exit status 1: must be less than or equal to 50.", ""},
			wantLimit: 50,
			wantOK:    true,
		},
		{
			name:      "unrelated error",
			parts:     []string{"exit status 1", "connection refused"},
			wantLimit: 0,
			wantOK:    false,
		},
		{
			name:      "no parts",
			parts:     nil,
			wantLimit: 0,
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLimit, gotOK := parsePrefectMaxLimit(tt.parts...)
			if gotLimit != tt.wantLimit || gotOK != tt.wantOK {
				t.Errorf("parsePrefectMaxLimit(%q) = (%d, %t), want (%d, %t)",
					tt.parts, gotLimit, gotOK, tt.wantLimit, tt.wantOK)
			}
		})
	}
}
