package residencezones

import "testing"

func TestParseRefusesMovesOutOfOrder(t *testing.T) {
	for _, spec := range []string{"", "Nowhere/Zone", "UTC,2022-01-01", "UTC,2022-05-01=UTC,2022-01-01=UTC"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) took it", spec)
		}
	}
}
