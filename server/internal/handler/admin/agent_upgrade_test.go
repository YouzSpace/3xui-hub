package admin

import "testing"

func TestAgentUpgradeOutdated(t *testing.T) {
	cases := []struct {
		name     string
		from     string
		latest   string
		reported string
		want     string
		outdated bool
	}{
		{"从未上报版本", "", "1.2.1", "", "", true},
		{"版本落后", "1.2.0", "1.2.1", "", "", true},
		{"版本足够、没报 sha（老 agent）", "1.2.1", "1.2.1", "", "abc", false},
		{"版本足够、sha 一致", "1.2.1", "1.2.1", "abc", "ABC", false},
		{"版本相同、sha 不一致（同版本重编译）", "1.2.1", "1.2.1", "old", "new", true},
		{"manifest 没 sha 时只比版本", "1.2.2", "1.2.1", "old", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentUpgradeOutdated(c.from, c.latest, c.reported, c.want); got != c.outdated {
				t.Fatalf("agentUpgradeOutdated(%q,%q,%q,%q) = %v, want %v",
					c.from, c.latest, c.reported, c.want, got, c.outdated)
			}
		})
	}
}
