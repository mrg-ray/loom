package contextoptimiser

import (
	"regexp"
	"strings"
	"testing"
)

// §Ladder: a still-long fold render must carry BOTH descent addresses — fold:N
// (recall the long) and msg:A-B (recall the exact messages) — appended by code,
// so the ladder is navigable regardless of what the compressor wrote.
func TestLadder_LongFoldCarriesDescentAddresses(t *testing.T) {
	requireGate(t)
	comp := &countingCompressor{summary: "opaque prose the compressor produced with no ids at all"}
	r := driveToFolds(t, "ladder-long", comp, 1)

	sys := systemMessages(renderedContext(t, r, "ladder-long"))
	if len(sys) < 2 {
		t.Fatal("no L2 block rendered")
	}
	block := sys[1].Content
	if !regexp.MustCompile(`fold:\d+`).MatchString(block) {
		t.Errorf("§ladder: long render lacks a fold:N address:\n%s", trim(block))
	}
	// The compressor's summary contains no ids, yet the render must carry msg:A-B.
	if !regexp.MustCompile(`msg:\d+-\d+`).MatchString(block) {
		t.Errorf("§ladder: long render lacks the msg:A-B range — descent to exact messages "+
			"depends on the LLM, not code:\n%s", trim(block))
	}
	if strings.Contains(block, "opaque prose") &&
		!regexp.MustCompile(`\[fold:\d+\] \(msg:\d+-\d+\)`).MatchString(block) {
		t.Errorf("§ladder: the descent address is not appended by code around the opaque summary:\n%s", trim(block))
	}
}
