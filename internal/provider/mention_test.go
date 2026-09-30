package provider

import "testing"

func TestWithMention(t *testing.T) {
	for _, c := range []struct{ body, author, want string }{
		{"Thanks, fixed in 1.2.", "thunderbeast", "@thunderbeast Thanks, fixed in 1.2."},
		{"@thunderbeast Thanks", "thunderbeast", "@thunderbeast Thanks"},
		{"  \n@ThunderBeast, thanks", "thunderbeast", "@ThunderBeast, thanks"},
		{"@thunderbeast", "thunderbeast", "@thunderbeast"},
		{"@thunderbeast2 hi", "thunderbeast", "@thunderbeast @thunderbeast2 hi"},
		{"@other hi", "thunderbeast", "@thunderbeast @other hi"},
		{"\t hi", "", "hi"},
		{"Спасибо!", "Гость", "@Гость Спасибо!"},
		{"@гость спасибо", "Гость", "@гость спасибо"},
	} {
		if got := WithMention(c.body, c.author); got != c.want {
			t.Errorf("WithMention(%q, %q) = %q, want %q", c.body, c.author, got, c.want)
		}
	}
}
