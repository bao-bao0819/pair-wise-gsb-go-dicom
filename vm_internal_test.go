package dicom

import "testing"

func TestParseVM(t *testing.T) {
	tests := []struct {
		vm       string
		count    int
		want     bool
		parsable bool
	}{
		{"1", 1, true, true},
		{"1", 2, false, true},
		{"3", 3, true, true},
		{"1-2", 1, true, true},
		{"1-2", 2, true, true},
		{"1-2", 3, false, true},
		{"2-4", 4, true, true},
		{"2-4", 5, false, true},
		{"1-n", 1, true, true},
		{"1-n", 500, true, true},
		{"2-n", 1, false, true},
		{"6-n", 6, true, true},
		{"1-99", 99, true, true},
		{"1-99", 100, false, true},
		{"2-2n", 2, true, true},
		{"2-2n", 4, true, true},
		{"2-2n", 6, true, true},
		{"2-2n", 1, false, true},
		{"2-2n", 3, false, true},
		{"3-3n", 9, true, true},
		{"3-3n", 6, true, true},
		{"3-3n", 4, false, true},
		{"1-n or 1", 1, true, true},
		{"1-n or 1", 7, true, true},
		{"weird", 1, false, false},
		{"", 1, false, false},
	}
	for _, tc := range tests {
		rules, ok := parseVM(tc.vm)
		if ok != tc.parsable {
			t.Errorf("parseVM(%q) parsable=%v, want %v", tc.vm, ok, tc.parsable)
			continue
		}
		if !ok {
			continue
		}
		got := false
		for _, r := range rules {
			if r.matches(tc.count) {
				got = true
				break
			}
		}
		if got != tc.want {
			t.Errorf("VM %q match(%d) = %v, want %v", tc.vm, tc.count, got, tc.want)
		}
	}
}
