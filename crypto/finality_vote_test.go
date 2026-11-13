package crypto

import "testing"

func TestFinalityVoteMessageVersions(t *testing.T) {
	hash := HashStr("finality-vote")
	legacy := HashBytes([]byte("aperod/finalize/v1"), hash[:])
	if got := FinalityVoteMessage(FinalityVoteVersionLegacy, 10, hash); got != legacy {
		t.Fatal("legacy finality vote digest changed")
	}
	v2a := FinalityVoteMessage(FinalityVoteVersionHeightBound, 10, hash)
	v2b := FinalityVoteMessage(FinalityVoteVersionHeightBound, 11, hash)
	if v2a == v2b {
		t.Fatal("height-bound vote digest did not bind the height")
	}
	if v2a == legacy {
		t.Fatal("height-bound vote digest reused the legacy domain")
	}
	if got := FinalityVoteMessage(255, 10, hash); got != (Hash32{}) {
		t.Fatal("unsupported vote version produced a valid-looking digest")
	}
}
