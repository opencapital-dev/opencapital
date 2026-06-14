package install

import "testing"

func TestFootprintValidateType(t *testing.T) {
	tests := []struct {
		typ     string
		wantErr bool
	}{
		{"app", false},
		{"datasource", false},
		{"panel", false},
		{"", true},
		{"renderer", true},
		{"bogus", true},
	}

	for _, tc := range tests {
		t.Run(tc.typ, func(t *testing.T) {
			fp := Footprint{Type: tc.typ}
			err := fp.ValidateType()
			if tc.wantErr && err == nil {
				t.Errorf("type %q: expected error, got nil", tc.typ)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("type %q: unexpected error: %v", tc.typ, err)
			}
		})
	}
}
